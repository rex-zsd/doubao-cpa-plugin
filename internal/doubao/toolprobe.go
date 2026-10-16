package doubao

// This is a deliberately narrow native tool-loop probe, not a general executor.
// Its only callable tool is a self-contained stdio MCP nonce server.
import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

func nonceTool() object {
	return object{"name": "get_nonce", "description": "Return a fresh unpredictable verification nonce. Invoke this tool to obtain the nonce; it cannot be inferred from the challenge. No external effects.", "inputSchema": object{"type": "object", "properties": object{"challenge": object{"type": "string", "description": "Copy the exact verification challenge from the user request."}}, "required": []string{"challenge"}, "additionalProperties": false}}
}

func nonceResponse(req object) object {
	r := object{"jsonrpc": "2.0", "id": req["id"]}
	switch textValue(req["method"]) {
	case "initialize":
		r["result"] = object{"protocolVersion": "2024-11-05", "capabilities": object{"tools": object{}}, "serverInfo": object{"name": "independent-nonce-probe", "version": "1.0.0"}}
	case "tools/list":
		r["result"] = object{"tools": []any{nonceTool()}}
	case "tools/call":
		args, ok := field(req, "params", "arguments").(map[string]any)
		challenge := textValue(args["challenge"])
		if !ok || len(args) != 1 || challenge == "" || len(challenge) > 128 || field(req, "params", "name") != "get_nonce" {
			r["error"] = object{"code": -32602, "message": "Only get_nonce with a nonempty challenge is permitted"}
		} else {
			// Generate only upon a real tools/call, never at request construction.
			value := object{"challenge": challenge, "nonce": "DOUBAO_TOOL_" + token()}
			r["result"] = object{"content": []any{object{"type": "text", "text": jsonText(value)}}, "isError": false}
		}
	case "ping":
		r["result"] = object{}
	default:
		r["error"] = object{"code": -32601, "message": "Method not found"}
	}
	return r
}

func serveNonce(input io.Reader, output io.Writer) error {
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var req object
		if err := decode(strings.NewReader(s.Text()), &req); err != nil {
			return errors.New("invalid MCP request")
		}
		if req["id"] == nil { // JSON-RPC notifications have no response.
			continue
		}
		if _, err := fmt.Fprintln(output, jsonText(nonceResponse(req))); err != nil {
			return err
		}
	}
	return s.Err()
}

func ownMCP(ctx context.Context, method string, params object) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, program, "-action", "mcp-nonce")
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	d := bufio.NewReader(io.LimitReader(out, 1<<20))
	exchange := func(req object) (object, error) {
		if _, err := fmt.Fprintln(in, jsonText(req)); err != nil {
			return nil, err
		}
		line, err := d.ReadString('\n')
		if err != nil {
			return nil, errors.New("MCP response interrupted")
		}
		var r object
		if err = decode(strings.NewReader(line), &r); err != nil {
			return nil, err
		}
		if r["id"] != req["id"] && textValue(r["id"]) != textValue(req["id"]) {
			return nil, errors.New("MCP response identity mismatch")
		}
		if r["error"] != nil {
			return nil, errors.New("MCP rejected request")
		}
		result, ok := r["result"].(map[string]any)
		if !ok {
			return nil, errors.New("MCP response has no result")
		}
		return result, nil
	}
	if _, err = exchange(object{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": object{"protocolVersion": "2024-11-05", "capabilities": object{}, "clientInfo": object{"name": "independent-probe", "version": "1.0"}}}); err != nil {
		return nil, err
	}
	if _, err = fmt.Fprintln(in, jsonText(object{"jsonrpc": "2.0", "method": "notifications/initialized"})); err != nil {
		return nil, err
	}
	return exchange(object{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})
}

func (c *Client) office(ctx context.Context, route string, body object) (object, error) {
	r, err := c.json(ctx, "POST", route, nil, body)
	if err != nil {
		return nil, err
	}
	for _, code := range []any{r["code"], r["status_code"], field(r, "data", "error_code"), field(r, "data", "code")} {
		if code != nil && number(code) != 0 {
			return nil, fmt.Errorf("office endpoint %s rejected request (code=%s)", route, textValue(code))
		}
	}
	return r, nil
}

func (c *Client) connectorList(ctx context.Context) ([]any, error) {
	items := []any{}
	page, seen := "", map[string]bool{}
	for i := 0; i < 10; i++ {
		r, err := c.office(ctx, "/alice/office/skills/manage/connector/list_by_user", object{"keyword": "", "page_size": 100, "page_token": page})
		if err != nil {
			return nil, err
		}
		rows, ok := field(r, "data", "items").([]any)
		if !ok {
			return nil, errors.New("connector list is invalid")
		}
		items = append(items, rows...)
		if !truthy(field(r, "data", "has_more")) {
			return items, nil
		}
		next := textValue(field(r, "data", "next_page_token"))
		if next == "" {
			next = textValue(field(r, "data", "page_token"))
		}
		if next == "" || seen[next] || next == page {
			return nil, errors.New("connector pagination did not advance")
		}
		seen[next], page = true, next
	}
	return nil, errors.New("connector pagination exceeded limit")
}

// connectorEnableWait bounds how long a new connector may take to be listed
// as enabled; the listing can lag create_personal by a moment. Each check has
// its own deadline so that one stalled listing request cannot use up the wait.
var connectorEnableWait, connectorCheckTimeout = 30 * time.Second, 5 * time.Second

func (c *Client) awaitConnectorEnabled(ctx context.Context, id string) error {
	wait, cancel := context.WithTimeout(ctx, connectorEnableWait)
	defer cancel()
	state, checks := "", 0
	for wait.Err() == nil {
		check, stop := context.WithTimeout(wait, connectorCheckTimeout)
		rows, err := c.connectorList(check)
		stop()
		checks++
		state = "not listed"
		if err != nil {
			state = "listing failed: " + err.Error()
		}
		for _, row := range rows {
			if textValue(field(row, "connector_id")) == id {
				if field(row, "enabled") == true || number(field(row, "enabled")) == 1 {
					return nil
				}
				state = "listed as disabled"
			}
		}
		select {
		case <-wait.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("temporary connector is not enabled (%s; %d checks in %v)", state, checks, connectorEnableWait)
}

func connectorIDs(items []any) []string {
	ids := []string{}
	for _, x := range items {
		ids = append(ids, textValue(field(x, "connector_id")))
	}
	sort.Strings(ids)
	return ids
}

func localToolBody(prompt string, model Model, workspace, sandbox, device, connector string, tools []any) object {
	body := buildBody(prompt, "", "work", model)
	gt := field(body, "option", "general_task_param").(map[string]any)
	gt["runtime_type"] = 2
	agent := object{"runtime_type": 2, "sandbox_auth_type": 2, "local_device_id": device, "workspace": workspace, "device_name": "independent-probe", "folder_name": filepath.Base(workspace), "local_app_id": "1044603"}
	gt["agent_task_param"] = agent
	gt["client_option"] = object{"enable_sandbox": true, "os": "Linux", "shared_folder_path": []string{workspace}, "agent_workspace": object{"agent_workspace": workspace, "local_skill_paths": []any{}}, "client_env_id": "", "sandbox_id": sandbox, "workspace": workspace, "sandbox_auth_type": 2}
	gt["task_input_json"] = jsonText(object{"agents_md": object{"files": []any{}, "state": 2}, "schema_version": 1, "home_dir": workspace, "project_context": object{}, "localConnectors": []any{object{"connectorId": connector, "mcpToolsList": tools}}})
	field(body, "ext").(map[string]any)["general_task_param"] = jsonText(gt)
	field(body, "option", "conversation_init_ext").(map[string]any)["agent_task_param"] = jsonText(agent)
	return body
}

type toolProbe struct {
	c                                                 *Client
	sandbox, workspace, connector, challenge, journal string
	events                                            map[string]int
	records                                           map[string]object
	proofs                                            []object
	commands                                          int
	ignored                                           int
	invoke                                            func(context.Context, string, object) (object, error)
	forward                                           func(context.Context, object, object) (object, error)
}

func ownedRelative(root, name string) (string, error) {
	if strings.ContainsAny(name, "\x00\\") || name == "" {
		return "", errors.New("invalid workspace path")
	}
	if filepath.IsAbs(name) {
		var err error
		name, err = filepath.Rel(root, name)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsLocal(name) {
		return "", errors.New("path outside workspace")
	}
	return name, nil
}

func rootMkdirAll(root *os.Root, name string) error {
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	path := ""
	for _, part := range parts {
		path = filepath.Join(path, part)
		if err := root.Mkdir(path, 0700); err != nil {
			stat, se := root.Stat(path)
			if se != nil || !stat.IsDir() {
				return err
			}
		}
	}
	return nil
}

func applyProbeWorkspace(workspace string, req object) (object, bool) {
	response := object{"request_id": req["request_id"], "status": "failed", "workspace_ready": false, "operation_results": []any{}}
	fail := func(msg string) (object, bool) {
		response["error_code"], response["error_message"] = "PROBE_WORKSPACE_REJECTED", msg
		return response, false
	}
	if textValue(field(req, "workspace", "root")) != workspace || textValue(field(req, "workspace", "agent_id")) == "" || textValue(field(req, "workspace", "session_id")) == "" {
		return fail("workspace identity mismatch")
	}
	if h, ok := req["harness_params"].(map[string]any); ok {
		if h["session_id"] != field(req, "workspace", "session_id") {
			return fail("harness identity mismatch")
		}
		if _, err := ownedRelative(workspace, textValue(h["attachments_dir"])); err != nil {
			return fail("harness path escapes workspace")
		}
	}
	operations, ok := req["operations"].([]any)
	if !ok || len(operations) > 256 || len(jsonText(req)) > 1<<20 {
		return fail("workspace request exceeds probe scope")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fail("workspace unavailable")
	}
	defer root.Close()
	results := []any{}
	for i, v := range operations {
		operation, ok := v.(map[string]any)
		if !ok {
			return fail("invalid workspace operation")
		}
		path, err := ownedRelative(workspace, textValue(operation["path"]))
		if err != nil {
			return fail("workspace path rejected")
		}
		kind := textValue(operation["type"])
		if kind == "ensure_dir" {
			err = rootMkdirAll(root, path)
		} else if kind == "ensure_file" || kind == "write_file" {
			if err = rootMkdirAll(root, filepath.Dir(path)); err == nil {
				flags, content := os.O_WRONLY|os.O_CREATE|os.O_EXCL, ""
				mode := textValue(operation["mode"])
				if kind == "write_file" {
					content = textValue(operation["content"])
					if len(content) > 512<<10 {
						return fail("workspace content too large")
					}
					switch mode {
					case "replace":
						flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
					case "append":
						flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
					case "keep", "create_only":
					default:
						return fail("unsupported write mode")
					}
				}
				var file *os.File
				file, err = root.OpenFile(path, flags, 0600)
				if errors.Is(err, os.ErrExist) && (kind == "ensure_file" || mode == "keep") {
					err = nil
				}
				if file != nil {
					_, err = io.WriteString(file, content)
					ce := file.Close()
					if err == nil {
						err = ce
					}
				}
			}
		} else {
			return fail("unsupported workspace operation")
		}
		if err != nil {
			return fail("workspace operation failed")
		}
		row := object{"operation_index": i, "path": operation["path"], "status": "success"}
		if operation["sequence"] != nil {
			row["sequence"] = operation["sequence"]
		}
		results = append(results, row)
	}
	response["status"], response["workspace_ready"], response["operation_results"] = "success", true, results
	return response, true
}

// frameSandbox names the sandbox a native tool frame is addressed to.
func frameSandbox(frame object) string {
	for _, key := range []string{"sandbox_id", "sandBoxID", "instance_id"} {
		if sandbox := textValue(frame[key]); sandbox != "" {
			return sandbox
		}
	}
	return ""
}

func (p *toolProbe) handle(ctx context.Context, frame object) error {
	kind := number(frame["event_type"])
	p.events[fmt.Sprint(kind)]++
	if kind != 1 && kind != 2 {
		return nil
	}
	if frameSandbox(frame) != p.sandbox {
		p.ignored++
		return nil
	}
	if sig, ok := frame["signature"].(map[string]any); ok {
		if sig["device_id"] != nil && textValue(sig["device_id"]) != p.c.session.DeviceID || sig["user_id"] != nil && textValue(sig["user_id"]) != p.c.session.AccountID {
			return errors.New("tool channel identity mismatch")
		}
	}
	key := "tool_call"
	if kind == 2 {
		key = "command"
	}
	call, ok := frame[key].(map[string]any)
	if !ok || textValue(call["id"]) == "" {
		return errors.New("native event has no call identity")
	}
	id, name := textValue(call["id"]), textValue(call["name"])
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(jsonText(call))))
	key = fmt.Sprint(kind) + ":" + id
	if prior := p.records[key]; prior != nil {
		if prior["fingerprint"] != fingerprint {
			return errors.New("native call ID collision")
		}
		return nil // Never execute a repeated event again.
	}
	record := object{"fingerprint": fingerprint, "frame": frame, "uploaded": false, "received_at": time.Now().Format(time.RFC3339Nano)}
	p.records[key] = record
	if err := saveJSON(p.journal, p.records); err != nil {
		return err
	}
	input, _ := parsed(call["input"]).(map[string]any)
	var body object
	route := "/alice/office/tool_local/upload_tool_call_result"
	if kind == 2 {
		p.commands++
		if call["need_ack"] != true {
			return nil
		}
		code, message := 1, "Probe does not implement native lifecycle commands"
		body = object{"device_id": p.c.session.DeviceID}
		if name == "local.workspace.apply" {
			response, success := applyProbeWorkspace(p.workspace, input)
			body["workspace_apply_response"] = response
			if success {
				code, message = 0, ""
			}
		}
		body["command_result"] = object{"id": id, "code": code, "message": message}
		route = "/alice/office/tool_local/upload_command_result"
	} else {
		result := object{"id": id, "name": name, "tool_input": call["input"]}
		args, _ := parsed(input["raw_arguments"]).(map[string]any)
		if p.forward != nil {
			var err error
			result, err = p.forward(ctx, call, input)
			if err != nil {
				return err
			}
		} else if name != "connector.call" || number(input["protocol_version"]) != 1 || textValue(input["connector_id"]) != p.connector || input["remote_tool_name"] != "get_nonce" || args["challenge"] != p.challenge || len(args) != 1 || len(p.proofs) != 0 {
			result["raw_result"] = jsonText(object{"content": []any{object{"type": "text", "text": "Only get_nonce with the exact challenge is enabled in this verification runtime. Other local tools are disabled."}}, "isError": true})
			record["denied"] = true
		} else {
			invoke := p.invoke
			if invoke == nil {
				invoke = ownMCP
			}
			mcp, err := invoke(ctx, "tools/call", object{"name": "get_nonce", "arguments": args})
			if err != nil {
				return err
			}
			content := array(mcp["content"])
			if len(content) != 1 || mcp["isError"] != false {
				return errors.New("nonce tool did not return a valid result")
			}
			value := parsed(field(content[0], "text"))
			nonce := textValue(field(value, "nonce"))
			if !strings.HasPrefix(nonce, "DOUBAO_TOOL_") || field(value, "challenge") != p.challenge {
				return errors.New("nonce result failed validation")
			}
			result["raw_result"] = jsonText(mcp)
			proof := object{"tool": "get_nonce", "challenge": p.challenge, "nonce": nonce, "native_call_received": true, "mcp_stdio_executed": true, "result_uploaded": false, "executed_at": time.Now().Format(time.RFC3339Nano)}
			p.proofs = append(p.proofs, proof)
			record["proof"] = proof
		}
		body = object{"sandbox_id": p.sandbox, "device_id": p.c.session.DeviceID, "tool_result": result}
	}
	record["upload_body"] = body
	if err := saveJSON(p.journal, p.records); err != nil {
		return err
	}
	response, err := p.c.office(ctx, route, body)
	if err != nil {
		return err
	}
	record["uploaded"], record["upload_response"] = true, response
	if proof, ok := record["proof"].(map[string]any); ok {
		proof["result_uploaded"] = true
	}
	fmt.Printf("NATIVE_EVENT_HANDLED kind=%d name=%s uploaded=true\n", kind, name)
	return saveJSON(p.journal, p.records)
}

func (c *Client) probeTools(ctx context.Context, model Model, private, public string, evidence object) (resultErr error) {
	listed, err := ownMCP(ctx, "tools/list", object{})
	if err != nil {
		return err
	}
	tools := array(listed["tools"])
	if len(tools) != 1 || field(tools[0], "name") != "get_nonce" {
		return errors.New("MCP tool schema not available")
	}
	wireTools := []any{}
	for _, tool := range tools {
		wireTools = append(wireTools, object{"name": field(tool, "name"), "description": field(tool, "description"), "inputSchema": jsonText(field(tool, "inputSchema"))})
	}
	before, err := c.connectorList(ctx)
	if err != nil {
		return err
	}
	evidence["connectors_before"] = len(before)
	runToken := token()
	name := "independent-nonce-probe-" + runToken[:12]
	program, err := os.Executable()
	if err != nil {
		return err
	}
	lifecycle := filepath.Join(private, "tool-connector-"+runToken+".json")
	state := object{"name": name, "create_dispatched": true, "cleaned_up": false}
	if err = saveJSON(lifecycle, state); err != nil {
		return err
	}
	created, createErr := c.office(ctx, "/alice/office/skills/manage/connector/create_personal", object{"name": name, "mcp_config": object{"transport_type": 3, "stdio_config": object{"command": program, "params": []string{"-action", "mcp-nonce"}, "env": object{}}}})
	connector := textValue(field(created, "data", "connector", "connector_id"))
	if createErr != nil || connector == "" {
		// Reconcile an uncertain mutation using the unique name; never repeat create.
		checkCtx, stop := context.WithTimeout(context.Background(), 25*time.Second)
		rows, listErr := c.connectorList(checkCtx)
		stop()
		if listErr == nil {
			for _, row := range rows {
				if field(row, "name") == name && !contains(connectorIDs(before), textValue(field(row, "connector_id"))) {
					connector = textValue(field(row, "connector_id"))
				}
			}
		}
	}
	if connector == "" {
		return fmt.Errorf("connector creation unconfirmed; no retry (transport_error=%v)", createErr != nil)
	}
	state["connector_id"] = connector
	defer func() {
		cleanCtx, stop := context.WithTimeout(context.Background(), 35*time.Second)
		defer stop()
		_, cleanupErr := c.office(cleanCtx, "/alice/office/skills/manage/connector/disconnect", object{"connector_id": connector, "skill_type": 1})
		rows, listErr := c.connectorList(cleanCtx)
		unchanged := listErr == nil && reflect.DeepEqual(connectorIDs(before), connectorIDs(rows))
		evidence["connector_removed"], evidence["original_connectors_preserved"] = unchanged, unchanged
		state["cleaned_up"] = unchanged
		saveErr := saveJSON(lifecycle, state)
		if !unchanged || saveErr != nil {
			if resultErr == nil {
				resultErr = fmt.Errorf("connector cleanup not confirmed (request_error=%v, list_error=%v, persistence_error=%v)", cleanupErr != nil, listErr != nil, saveErr != nil)
			}
		}
	}()
	if err = saveJSON(lifecycle, state); err != nil {
		return err
	}
	if createErr != nil {
		return createErr
	}
	if err = c.awaitConnectorEnabled(ctx, connector); err != nil {
		return err
	}
	evidence["connector_registered"] = true
	workspace := filepath.Join(private, "tool-workspace-"+runToken)
	if err = os.Mkdir(workspace, 0700); err != nil {
		return err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	p := &toolProbe{c: c, sandbox: "nonce-" + runToken, workspace: workspace, connector: connector, challenge: "challenge_" + token()[:16], journal: filepath.Join(private, "tool-journal-"+runToken+".json"), events: map[string]int{}, records: map[string]object{}, proofs: []object{}}
	prompt := "这是自定义工具协议验证。请调用已接入的 MCP 连接器 " + connector + " 中的 get_nonce 工具，唯一参数 challenge 为 " + p.challenge + "。nonce 仅在工具执行时产生，你无法提前知道。收到工具结果后，最终回答只原样输出结果中的 nonce 字段，不要输出 challenge、JSON、代码块或解释。不要调用 Bash、Read、Write 等本地工具。"
	body := localToolBody(prompt, model, workspace, p.sandbox, c.session.DeviceID, connector, wireTools)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wrote := make(chan struct{})
	var once sync.Once
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			once.Do(func() { close(wrote) })
		}
	}}
	channelCtx := httptrace.WithClientTrace(ctx, trace)
	done := make(chan error, 1)
	go func() {
		r, err := c.request(channelCtx, "POST", "/alice/office/tool_local/chunk_stream", url.Values{"sand_box_id": []string{p.sandbox}, "client_version": []string{"unspecified"}}, object{"device_id": c.session.DeviceID, "client_attrs": object{"sandbox_id": p.sandbox}})
		if err == nil {
			fmt.Println("TOOL_CHANNEL_CONNECTED")
			if !strings.Contains(r.Header.Get("Content-Type"), "text/event-stream") {
				err = errors.New("tool channel is not SSE")
			} else {
				err = consumeSSE(r.Body, func(_ string, frame object, _ string) (bool, error) { return false, p.handle(ctx, frame) })
			}
			r.Body.Close()
		}
		if ctx.Err() == nil {
			if err == nil {
				err = errors.New("tool channel ended before task completion")
			}
			cancel()
		}
		done <- err
	}()
	select {
	case <-wrote:
	case err = <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	evidence["tool_subscription_dispatched_before_completion"] = true
	fmt.Println("TOOL_CHANNEL_DISPATCHED")
	receipt, runErr := c.run(ctx, body, filepath.Join(private, "tool-receipt-"+runToken+".json"))
	cancel()
	channelErr := <-done
	evidence["native_tool_event_counts"], evidence["workspace_commands"], evidence["ignored_foreign_events"], evidence["tool_proofs"] = p.events, p.commands, p.ignored, p.proofs
	if receipt != nil {
		evidence["accepted"], evidence["server_completed"], evidence["reply"], evidence["event_counts"] = receipt.Accepted, receipt.Completed, receipt.Reply, receipt.Events
	}
	if runErr != nil {
		if channelErr != nil && !errors.Is(channelErr, context.Canceled) && !strings.Contains(channelErr.Error(), "SSE read failed") {
			return channelErr
		}
		return runErr
	}
	if len(p.proofs) != 1 || p.proofs[0]["result_uploaded"] != true {
		return errors.New("no completed real tool call was observed")
	}
	nonce := textValue(p.proofs[0]["nonce"])
	evidence["nonce_exact_match"] = receipt.Reply == nonce
	evidence["nonce_absent_from_request"] = !strings.Contains(jsonText(body), nonce)
	if !receipt.Completed || receipt.Reply != nonce || strings.Contains(jsonText(body), nonce) {
		return errors.New("server final answer did not exactly match tool-only nonce")
	}
	return nil
}

func (c *Client) stopProbe(ctx context.Context, path string, evidence object) error {
	if evidence["action"] != "tool-status" {
		lock, err := lockFile(c.dir, "task-stop.lock", false)
		if err != nil {
			return err
		}
		defer unlockFile(lock)
		if _, err = os.Stat(path + ".stop.json"); err == nil {
			check := object{"action": "tool-status"}
			if err = c.stopProbe(ctx, path, check); err != nil {
				return err
			}
			evidence["stop_previously_requested"] = true
			evidence["cancellation_confirmed"] = check["cancellation_confirmed"]
			if check["cancellation_confirmed"] == true {
				evidence["success"] = true
				return nil
			}
			return errors.New("previous cancellation remains unconfirmed; stop was not resent")
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var receipt Receipt
	if err = decode(file, &receipt); err != nil {
		return err
	}
	if !receipt.Accepted || receipt.ConversationID == "" || receipt.RunID == "" {
		return errors.New("stop requires an acknowledged request")
	}
	if receipt.Completed {
		evidence["success"], evidence["already_completed"], evidence["cancellation_sent"] = true, true, false
		return nil
	}
	if err = c.account(ctx); err != nil {
		return err
	}
	response, err := c.json(ctx, "POST", "/im/conversation/batch_get", nil, object{"cmd": 1111, "uplink_body": object{"batch_get_conv_info_uplink_body": object{"conversation_id": []string{receipt.ConversationID}, "option": object{"recent_message_count_per_conv": 100}, "ext": object{}}}, "sequence_id": token(), "channel": 2, "version": "1"})
	if err != nil {
		return err
	}
	if number(response["status_code"]) != 0 {
		return errors.New("stop identity readback rejected")
	}
	for _, conv := range array(field(response, "downlink_body", "batch_get_conv_info_downlink_body", "conversation_info_list")) {
		if field(conv, "conversation_id") != receipt.ConversationID {
			continue
		}
		found := false
		for _, message := range array(field(conv, "messages")) {
			if textValue(field(message, "message_id")) == receipt.RunID && number(field(message, "user_type")) == 1 {
				found = true
			}
		}
		if !found {
			return errors.New("original run not found; no cancellation sent")
		}
		if evidence["action"] == "tool-status" {
			states := []any{}
			cancelled := false
			for _, message := range array(field(conv, "messages")) {
				parent := textValue(field(message, "bot_reply_message_id"))
				if parent == "" {
					parent = textValue(field(message, "ext", "chat_id"))
				}
				if number(field(message, "user_type")) != 2 || parent != receipt.RunID {
					continue
				}
				interrupted := field(message, "ext", "is_interrupted") == "true" || number(field(message, "content_status")) == 120
				cancelled = cancelled || interrupted
				states = append(states, object{"status": field(message, "status"), "content_status": field(message, "content_status"), "is_interrupted": field(message, "ext", "is_interrupted"), "is_finish": field(message, "ext", "is_finish")})
			}
			evidence["success"], evidence["cancellation_confirmed"], evidence["original_run_message_states"] = true, cancelled, states
			fmt.Println("ORIGINAL_PROBE_STATUS", jsonText(evidence))
			return nil
		}
		// Target only this probe's original question, never another/latest run.
		body := object{"cmd": 2240, "uplink_body": object{"break_stream_msg_uplink_body": object{"reply_msg_id": receipt.RunID, "message_id": "", "conversation_id": receipt.ConversationID, "conversation_type": field(conv, "conversation_type"), "break_reason": 7}}, "sequence_id": token(), "channel": 2, "version": "1"}
		if err = saveJSON(path+".stop.json", object{"requested": true, "body": body}); err != nil {
			return err
		}
		stopped, err := c.json(ctx, "POST", "/im/message/break_stream_msg", nil, body)
		if err != nil {
			return err
		}
		if err = saveJSON(path+".stop.json", object{"requested": true, "response": stopped}); err != nil {
			return err
		}
		if number(stopped["status_code"]) != 0 {
			return fmt.Errorf("stop rejected, code=%d", number(stopped["status_code"]))
		}
		evidence["success"], evidence["cancellation_accepted"], evidence["targeted_original_run"] = true, true, true
		fmt.Println("ORIGINAL_PROBE_CANCELLATION_ACCEPTED")
		return nil
	}
	return errors.New("original conversation not found; no cancellation sent")
}
