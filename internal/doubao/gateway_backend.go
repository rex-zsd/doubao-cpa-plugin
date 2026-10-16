package doubao

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The native agent consumes a MCP-shaped catalog. Client functions themselves
// remain outside this process: forwardClientTool waits for a later HTTP request.
func serveToolDescriptor(input io.Reader, output io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var tools []any
	if err = decode(io.LimitReader(file, 2<<20), &tools); err != nil {
		return err
	}
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var req object
		if err = decode(strings.NewReader(s.Text()), &req); err != nil {
			return err
		}
		if req["id"] == nil {
			continue
		}
		reply := object{"jsonrpc": "2.0", "id": req["id"]}
		switch req["method"] {
		case "tools/list":
			reply["result"] = object{"tools": tools}
		case "tools/call":
			reply["result"] = object{"content": []any{object{"type": "text", "text": "Client functions are executed by the HTTP caller, not by this descriptor process."}}, "isError": true}
		default:
			reply = nonceResponse(req)
		}
		if _, err = fmt.Fprintln(output, jsonText(reply)); err != nil {
			return err
		}
	}
	return s.Err()
}

func renderAPIPrompt(req apiRequest, connector string) string {
	prompt := ""
	if len(req.Messages) == 1 && req.Messages[0].Role == "user" {
		prompt = textValue(req.Messages[0].Content)
	} else {
		// Work has no verified native system/history import endpoint. Preserve all
		// roles as an explicit transcript, without claiming system-role priority.
		prompt = "下面 JSON 是调用方提供的文本对话记录。请根据全部上下文，回答最后一条用户消息：\n" + jsonText(req.Messages)
	}
	if connector != "" {
		prompt = "外部函数已通过 MCP 连接器 " + connector + " 注册。需要使用函数时，请调用该连接器中对应的工具，并等待真实结果。函数由调用方执行。不要调用本地 Bash、Read、Write、Edit 等工具。不要把工具调用模拟成普通文本 JSON。\n\n" + prompt
		if parallelRequested(req) {
			prompt = "当前连接器只有 client_tools_batch 工具。对相互独立的函数，请一次调用 client_tools_batch，把所有函数名和参数放入 calls 数组。调用方会并行执行这些函数。若某个函数依赖上一步结果，请下一批再提交。必须发起真实 MCP 调用，不能只输出计划。\n" + prompt
		}
	}
	return prompt
}

func (g *gateway) newConnector(t *gatewayTurn, tools []any) (string, error) {
	program, err := os.Executable()
	if err != nil {
		return "", err
	}
	if g.helperPath != "" {
		program = g.helperPath
	}
	path := filepath.Join(g.dir, "gateway-schema-"+t.id+".json")
	// The descriptor process serves ordinary MCP schemas, not the native DTO's
	// JSON-encoded inputSchema field.
	descriptions := []any{}
	for _, tool := range tools {
		descriptions = append(descriptions, object{"name": field(tool, "name"), "description": field(tool, "description"), "inputSchema": parsed(field(tool, "inputSchema"))})
	}
	if err = saveJSON(path, descriptions); err != nil {
		return "", err
	}
	name := "independent-http-tools-" + t.id[:12]
	result, createErr := g.c.office(t.ctx, "/alice/office/skills/manage/connector/create_personal", object{"name": name, "mcp_config": object{"transport_type": 3, "stdio_config": object{"command": program, "params": []string{"-action", "mcp-describe", "-tool-schema", path}, "env": object{}}}})
	id := textValue(field(result, "data", "connector", "connector_id"))
	if id == "" {
		// Reconcile uncertain creation by the unique task name. Never resend it.
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		rows, listErr := g.c.connectorList(ctx)
		cancel()
		if listErr == nil {
			for _, row := range rows {
				if field(row, "name") == name {
					id = textValue(field(row, "connector_id"))
				}
			}
		}
	}
	if id == "" {
		return "", errors.New("temporary connector creation was not confirmed; creation was not retried")
	}
	g.mu.Lock()
	t.connector = id
	saveErr := g.checkpointLocked(t)
	g.mu.Unlock()
	if saveErr != nil {
		return id, saveErr
	}
	if createErr != nil {
		return id, createErr
	}
	rows, err := g.c.connectorList(t.ctx)
	if err != nil {
		return id, err
	}
	for _, row := range rows {
		if textValue(field(row, "connector_id")) == id && (field(row, "enabled") == true || number(field(row, "enabled")) == 1) {
			return id, nil
		}
	}
	return id, errors.New("temporary connector is not enabled")
}

func (g *gateway) removeConnector(t *gatewayTurn) error {
	g.mu.Lock()
	id := t.connector
	alreadyRequested := t.cleanupRequested
	g.mu.Unlock()
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if !alreadyRequested {
		g.mu.Lock()
		t.cleanupRequested = true
		err := g.checkpointLocked(t)
		g.mu.Unlock()
		if err != nil {
			return err
		}
		// Persist before the mutation; a disconnect or timeout is never retried.
		_, _ = g.c.office(ctx, "/alice/office/skills/manage/connector/disconnect", object{"connector_id": id, "skill_type": 1})
	}
	for {
		rows, listErr := g.c.connectorList(ctx)
		if listErr != nil {
			return errors.New("connector cleanup could not be checked")
		}
		present := false
		for _, row := range rows {
			if textValue(field(row, "connector_id")) == id {
				present = true
			}
		}
		if !present {
			g.mu.Lock()
			t.cleanupConfirmed = true
			err := g.checkpointLocked(t)
			g.mu.Unlock()
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("connector removal is still pending")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func awaitAccepted(ctx context.Context, path string) error {
	for {
		file, err := os.Open(path)
		if err == nil {
			var receipt Receipt
			err = decode(file, &receipt)
			file.Close()
			if err == nil && receipt.Accepted && receipt.ConversationID != "" && receipt.RunID != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func nativeToolError(call object, message string) object {
	return object{"id": call["id"], "name": call["name"], "tool_input": call["input"], "content": []any{object{"type": "text", "text": message}}, "isError": true, "error_code": "CLIENT_TOOL_ONLY", "error_msg": message}
}

func (g *gateway) forwardClientTool(ctx context.Context, t *gatewayTurn, call, input object) (object, error) {
	g.mu.Lock()
	initial := t.request
	connector := t.connector
	g.mu.Unlock()
	name := textValue(input["remote_tool_name"])
	isBatch := parallelRequested(initial) && name == batchToolName
	allowed := false
	for _, tool := range initial.Tools {
		if tool.Function.Name == name {
			allowed = true
		}
	}
	args, valid := parsed(input["raw_arguments"]).(map[string]any)
	if textValue(call["name"]) != "connector.call" || number(input["protocol_version"]) != 1 || textValue(input["connector_id"]) != connector || !(allowed || isBatch) || !valid {
		g.mu.Lock()
		t.denied++
		tooMany := t.denied > 16
		g.mu.Unlock()
		if tooMany {
			return nil, errors.New("upstream repeatedly requested unavailable local tools")
		}
		return nativeToolError(call, "Only functions declared by the HTTP caller are available. Use the registered MCP connector; local shell/file tools are disabled."), nil
	}
	if pending := g.findNativePending(t, textValue(call["id"])); pending != nil {
		return g.awaitNativePending(ctx, t, pending, call)
	}
	if isBatch {
		return g.forwardClientBatch(ctx, t, call, args, initial)
	}
	if err := awaitAccepted(ctx, t.receiptPath); err != nil {
		return nil, err
	}
	publicCall := apiToolCall{ID: "call_" + token(), Type: "function", Function: apiCallFunction{Name: name, Arguments: jsonText(args)}}
	g.mu.Lock()
	t.forwarded++
	if t.forwarded > 32 {
		g.mu.Unlock()
		return nil, errors.New("tool chain exceeded 32 calls")
	}
	x := t.current
	message := apiMessage{Role: "assistant", Content: nil, ToolCalls: []apiToolCall{publicCall}}
	expected := append(append([]apiMessage{}, t.request.Messages...), message)
	p := &clientToolPending{turn: t, nativeID: textValue(call["id"]), exposed: x, call: publicCall, expected: expected, result: make(chan string, 1)}
	g.calls[publicCall.ID] = p
	if err := g.checkpointLocked(t); err != nil {
		delete(g.calls, publicCall.ID)
		g.mu.Unlock()
		return nil, err
	}
	g.mu.Unlock()
	x.resolve(message, "tool_calls", nil)
	var value string
	select {
	case value = <-p.result:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.toolReply(ctx, t, call, value, false)
}

func (g *gateway) runNative(t *gatewayTurn) {
	g.mu.Lock()
	req := t.request
	g.mu.Unlock()
	model, _, err := selectModel(g.models, req.Model, "work")
	var receipt *Receipt
	if err == nil && (len(req.Tools) == 0 || req.ToolChoice == "none") {
		var records []any
		req, records, err = g.c.prepareAPIMedia(t.ctx, req)
		body := buildBody(renderAPIPrompt(req, ""), "", "work", model)
		if len(records) > 0 {
			appendMedia(body, records)
		}
		g.mu.Lock()
		if err == nil {
			t.generationPosts++
			err = g.checkpointLocked(t)
		}
		g.mu.Unlock()
		if err == nil {
			receipt, err = g.c.runObserved(t.ctx, body, t.receiptPath, g.answerObserver(t))
		}
	} else if err == nil {
		receipt, err = g.runWithTools(t, req, model)
	}
	if err != nil {
		g.mu.Lock()
		suspending := g.suspending
		g.mu.Unlock()
		if suspending {
			return
		}
		// A deadline/disconnect is not permission to submit another generation.
		// Request cancellation only for this exact saved receipt, when accepted.
		if receipt != nil && receipt.Accepted && !receipt.Completed {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			stopErr := g.c.stopProbe(ctx, t.receiptPath, object{"action": "tool-stop"})
			cancel()
			if stopErr != nil {
				fmt.Fprintln(os.Stderr, "GATEWAY_CANCELLATION_UNCONFIRMED")
			}
		}
	}
	cleanupErr := g.removeConnector(t)
	if cleanupErr != nil {
		// Cleanup status is independent of the verified model answer.
		fmt.Fprintln(os.Stderr, "GATEWAY_CONNECTOR_CLEANUP_PENDING", cleanupErr.Error())
	}
	if err != nil {
		var inputFailure *apiError
		if t.generationPosts == 0 && errors.As(err, &inputFailure) {
			g.finish(t, "", inputFailure)
			return
		}
		fmt.Fprintln(os.Stderr, "GATEWAY_TASK_FAILED", err.Error())
		status, code := 409, "upstream_failed"
		if errors.Is(t.ctx.Err(), context.DeadlineExceeded) {
			status, code = 504, "task_expired"
		}
		g.finish(t, "", apiFail(status, code, "Upstream task did not complete. An accepted generation was not resubmitted; inspect the private receipt before retrying."))
		return
	}
	if receipt == nil || !receipt.Completed {
		g.finish(t, "", apiFail(502, "unconfirmed_completion", "No verified final upstream answer"))
		return
	}
	g.finish(t, receipt.Reply, nil)
}

func (g *gateway) runWithTools(t *gatewayTurn, req apiRequest, model Model) (*Receipt, error) {
	tools := []any{}
	for _, tool := range req.Tools {
		tools = append(tools, object{"name": tool.Function.Name, "description": tool.Function.Description, "inputSchema": jsonText(tool.Function.Parameters)})
	}
	if parallelRequested(req) {
		tools = batchCatalog(req.Tools)
	}
	connector, err := g.newConnector(t, tools)
	if err != nil {
		return nil, err
	}
	workspace := filepath.Join(g.dir, "gateway-workspace-"+t.id)
	if err = os.Mkdir(workspace, 0700); err != nil {
		return nil, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	sandbox := "http-" + t.id
	g.mu.Lock()
	t.sandbox = sandbox
	err = g.checkpointLocked(t)
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p := &toolProbe{c: g.c, sandbox: sandbox, workspace: workspace, connector: connector, journal: filepath.Join(g.dir, "gateway-native-"+t.id+".json"), events: map[string]int{}, records: map[string]object{}, forward: func(ctx context.Context, call, input object) (object, error) {
		return g.forwardClientTool(ctx, t, call, input)
	}}
	body := localToolBody(renderAPIPrompt(req, connector), model, workspace, sandbox, g.c.session.DeviceID, connector, tools)
	prepared, records, err := g.c.prepareAPIMedia(t.ctx, req)
	if err != nil {
		return nil, err
	}
	body = localToolBody(renderAPIPrompt(prepared, connector), model, workspace, sandbox, g.c.session.DeviceID, connector, tools)
	if len(records) > 0 {
		appendMedia(body, records)
	}
	ctx, cancel := context.WithCancel(t.ctx)
	defer cancel()
	wrote := make(chan struct{})
	var once sync.Once
	channelCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			once.Do(func() { close(wrote) })
		}
	}})
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	go func() {
		response, streamErr := g.c.request(channelCtx, "POST", "/alice/office/tool_local/chunk_stream", url.Values{"sand_box_id": []string{sandbox}, "client_version": []string{"unspecified"}}, object{"device_id": g.c.session.DeviceID, "client_attrs": object{"sandbox_id": sandbox}})
		if streamErr == nil {
			if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				streamErr = errors.New("native tool channel is not SSE")
			} else {
				streamErr = consumeSSE(response.Body, func(_ string, frame object, _ string) (bool, error) { return false, p.handle(ctx, frame) })
			}
			response.Body.Close()
		}
		if ctx.Err() == nil {
			if streamErr == nil {
				streamErr = errors.New("native tool stream ended before completion")
			}
			cancel()
		}
		done <- streamErr
	}()
	select {
	case <-wrote:
	case err = <-done:
		joined = true
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.mu.Lock()
	t.generationPosts++
	err = g.checkpointLocked(t)
	g.mu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	receipt, runErr := g.c.run(ctx, body, t.receiptPath)
	cancel()
	streamErr := <-done
	joined = true
	if runErr != nil {
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) && streamErr.Error() != "SSE read failed" {
			return receipt, streamErr
		}
		return receipt, runErr
	}
	return receipt, nil
}

// Read-only remote audit of our own saved experiments. Never issues connector
// mutations and never exports account, device, conversation or native call IDs.
func (c *Client) auditGateway(ctx context.Context, dir string, evidence object) error {
	if err := c.account(ctx); err != nil {
		return err
	}
	rows, err := c.connectorList(ctx)
	if err != nil {
		return err
	}
	ids := connectorIDs(rows)
	paths, err := filepath.Glob(filepath.Join(dir, "gateway-state-*.json"))
	if err != nil {
		return err
	}
	turns := []any{}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		var state object
		err = decode(file, &state)
		file.Close()
		if err != nil {
			return err
		}
		id := textValue(state["id"])
		if filepath.Base(path) != "gateway-state-"+id+".json" {
			return errors.New("gateway state identity mismatch")
		}
		var receipt Receipt
		file, err = os.Open(filepath.Join(dir, "gateway-receipt-"+id+".json"))
		if err == nil {
			err = decode(file, &receipt)
			file.Close()
			if err != nil {
				return err
			}
		} else if number(state["generation_posts"]) > 0 {
			return err
		}
		connector := textValue(state["connector_id"])
		removed := connector == "" || !contains(ids, connector)
		var native object
		if file, err = os.Open(filepath.Join(dir, "gateway-native-"+id+".json")); err == nil {
			err = decode(file, &native)
			file.Close()
			if err != nil {
				return err
			}
		}
		uploads := 0
		for _, record := range native {
			if number(field(record, "frame", "event_type")) == 1 && field(record, "uploaded") == true {
				uploads++
			}
		}
		messages := array(field(state, "request", "messages"))
		proof := object{"model": field(state, "request", "model"), "generation_posts": state["generation_posts"], "forwarded_tools": state["forwarded_tools"], "confirmed_native_tool_uploads": uploads, "server_completed": receipt.Completed, "api_completed": state["done"] == true && state["error"] == nil, "connector_removed": removed}
		if field(state, "request", "parallel_tool_calls") == true {
			proof["batch_adapter"] = true
			proof["final_answer_sha256"] = mediaHash([]byte(receipt.Reply))
			proof["denied_tool_requests"] = state["denied_tools"]
		} else if len(messages) > 0 && field(messages[len(messages)-1], "role") == "tool" {
			value := parsed(field(messages[len(messages)-1], "content"))
			expected := textValue(field(value, "ack"))
			if expected == "" {
				expected = textValue(field(value, "nonce"))
			}
			proof["final_exact_match"] = expected != "" && receipt.Reply == expected
			proof["value_absent_from_generation_request"] = expected != "" && !strings.Contains(jsonText(receipt.Request), expected)
		}
		turns = append(turns, proof)
	}
	evidence["success"], evidence["turns"], evidence["current_connector_count"], evidence["remote_mutations"] = true, turns, len(rows), 0
	fmt.Println("GATEWAY_AUDIT", jsonText(evidence))
	return nil
}
