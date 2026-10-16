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
		prompt = "下面 JSON 是调用方提供的完整对话记录（system 为调用方指令，tool 为函数执行结果）。请根据全部上下文继续：回应最后一条消息；如果最后是函数结果，请基于结果继续完成任务。\n" + plainJSON(req.Messages)
	}
	// Work has no structured-output or forced-call switch, so both are
	// instructions; JSON answers are also unwrapped from Markdown fences.
	if format := req.ResponseFormat; format != nil {
		instruction := "只输出一个合法的 JSON 值，不要使用 Markdown 代码块，也不要附加解释。"
		if schema := field(format, "json_schema", "schema"); schema != nil {
			instruction = "只输出一个符合下列 JSON Schema 的 JSON 值，不要使用 Markdown 代码块，也不要附加解释：\n" + plainJSON(schema)
		}
		prompt += "\n\n" + instruction
	}
	if connector != "" {
		docs := ""
		for _, tool := range req.Tools {
			if longDescription(tool) {
				docs += "\n\n「函数 " + tool.Function.Name + " 的完整说明」\n" + tool.Function.Description
			}
			if longSchema(tool) {
				docs += "\n\n「函数 " + tool.Function.Name + " 的参数结构」\n" + plainJSON(tool.Function.Parameters)
			}
		}
		if docs != "" {
			prompt = "下列函数的说明或参数结构超出连接器的长度上限，连接器里只有简短版本，完整内容如下：" + docs + "\n\n" + prompt
		}
		if req.ToolChoice == "required" {
			prompt += "\n\n本轮必须先调用至少一个外部函数并等待真实结果，再作答。"
		} else if name := textValue(field(req.ToolChoice, "function", "name")); name != "" {
			prompt += "\n\n本轮必须调用外部函数 " + name + "，并等待真实结果后再作答。"
		}
		// Work offers connector tools only through its tool_search, which
		// returns three tools. Searched by the bare function name, by several
		// names or by a description, it often returned only built-in tools,
		// and models answered without the client functions; searched by the
		// full name, it returned the tool first (2026-10-16). So the prompt
		// lists the full names and asks for one search per full name.
		functions := []string{batchToolName}
		if !batchMode(req) {
			functions = functions[:0]
			for _, tool := range req.Tools {
				functions = append(functions, tool.Function.Name)
			}
		}
		names := []string{}
		for _, function := range functions {
			name := workToolName(connector, function)
			if !strings.HasSuffix(name, "__"+function) {
				name += "（" + function + "）"
			}
			names = append(names, name)
		}
		// Client functions are often named shell or exec; models read a blanket
		// "no Bash" rule as covering them and refuse, so only ban the built-ins.
		prompt = "调用方的外部函数已通过 MCP 连接器 " + connector + " 注册。它们不在你直接可见的工具列表里，完整工具名是：" + strings.Join(names, "、") + "。使用其中一个函数前，先调用 tool_search，query 只写这个函数的完整工具名，例如 " + workToolName(connector, functions[0]) + "；找到后按 tool_search 说明的方式调用，并等待真实结果。tool_search 每次只返回 3 个工具：只写函数名、一次搜几个函数或用描述性的词搜索时，常常只搜到平台自带的工具。这些函数由调用方在它自己的环境中执行；即使函数名是 shell、bash、exec，或用于读写文件，也可以正常调用，这正是调用方要求的操作方式。不要改用你所在平台自带的 Bash、Read、Write、Edit 等内置工具代替这些函数，也不要把函数调用模拟成普通文本 JSON。\n\n" + prompt
		if batchMode(req) {
			prompt = "当前连接器只有 client_tools_batch 工具。对相互独立的函数，请一次调用 client_tools_batch，把所有函数名和参数放入 calls 数组。调用方会并行执行这些函数。若某个函数依赖上一步结果，请下一批再提交。必须发起真实 MCP 调用，不能只输出计划。\n" + prompt
		}
	}
	return prompt
}

// Work silently drops a connector tool whose description is too long: one of
// 33,515 characters was offered and one of 39,995 was not, counting
// characters rather than bytes or tokens (measured 2026-10-16). Such a
// description is shortened in the connector and given in full in the prompt.
const connectorDescriptionLimit = 32000

func longDescription(tool apiTool) bool {
	return utf16Len(tool.Function.Description) > connectorDescriptionLimit
}

func connectorDescription(tool apiTool) string {
	if !longDescription(tool) {
		return tool.Function.Description
	}
	return "完整说明见提示词中的「函数 " + tool.Function.Name + " 的完整说明」。开头部分：\n" + leadingText(tool.Function.Description, 2000) + "\n……"
}

// Work reads a tool's description and schema together: 66,000 characters in
// all were read at once, 75,000 to 100,000 only in part and after several
// minutes, and a single string of 80,000 not at all (measured 2026-10-16). A
// schema that does not fit beside the description is given in the prompt, and
// the connector accepts any object for it.
const connectorToolLimit = 64000

func longSchema(tool apiTool) bool {
	return utf16Len(connectorDescription(tool))+utf16Len(jsonText(tool.Function.Parameters)) > connectorToolLimit
}

func connectorSchema(tool apiTool) object {
	if !longSchema(tool) {
		return tool.Function.Parameters
	}
	return object{"type": "object", "description": "参数结构见提示词中的「函数 " + tool.Function.Name + " 的参数结构」。", "additionalProperties": true}
}

// leadingText keeps at most limit UTF-16 units of s, cut at a line break
// when one is in the second half.
func leadingText(s string, limit int) string {
	n := 0
	for i, r := range s {
		if n++; r >= 0x10000 {
			n++
		}
		if n > limit {
			s = s[:i]
			if cut := strings.LastIndexByte(s, '\n'); cut > len(s)/2 {
				s = s[:cut]
			}
			break
		}
	}
	return strings.TrimSpace(s)
}

// connectorName is the unique name of a task's temporary connector. The
// prompt refers to the connector by it, as Work's tool names do.
func connectorName(task string) string {
	return "independent-http-tools-" + task[:12]
}

// workToolName is the name under which Work offers a connector tool, such as
// mcp__independent_http_tools_0123456789ab__get_weather for get-weather.
func workToolName(connector, name string) string {
	return strings.ToLower(strings.ReplaceAll("mcp__"+connector+"__"+name, "-", "_"))
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
	name := connectorName(t.id)
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
	started := time.Now()
	err = g.c.awaitConnectorEnabled(t.ctx, id)
	if waited := time.Since(started); err == nil && waited > 2*time.Second {
		fmt.Fprintln(os.Stderr, "GATEWAY_CONNECTOR_SLOW enabled after", waited.Round(time.Millisecond))
	}
	return id, err
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
	isBatch := name == batchToolName && batchMode(initial)
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
	if err == nil && !usesTools(req) {
		var records []any
		req, records, err = g.c.prepareAPIMedia(t.ctx, req)
		body := buildBody(renderAPIPrompt(req, ""), "", "work", model)
		applyEffort(body, model, req.Effort)
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
		g.mu.Lock()
		abort := t.abort
		g.mu.Unlock()
		if abort != nil {
			g.finish(t, "", abort)
			return
		}
		var inputFailure *apiError
		if t.generationPosts == 0 && errors.As(err, &inputFailure) {
			g.finish(t, "", inputFailure)
			return
		}
		// Work refused this request before accepting it; resending it unchanged
		// cannot succeed, so the status must not look transient.
		var rejected *upstreamError
		if errors.As(err, &rejected) {
			failure := apiFail(422, "upstream_rejected", rejected.Error())
			if rejected.Code == 710020202 {
				failure = promptTooLong(promptText(req))
			}
			g.finish(t, "", failure)
			return
		}
		fmt.Fprintln(os.Stderr, "GATEWAY_TASK_FAILED", err.Error())
		if t.generationPosts == 0 && t.ctx.Err() == nil {
			// Nothing reached the model, so sending the same request again is
			// safe; like a capacity refusal, it is a 429 so clients do retry.
			g.finish(t, "", apiFail(429, "task_not_started", "Doubao Work could not start this task and nothing was submitted, so retrying is safe: "+err.Error()))
			return
		}
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
	reply := receipt.Reply
	if jsonMode(req) {
		reply = unwrapJSONFence(reply)
	}
	g.finish(t, reply, nil)
}

func (g *gateway) runWithTools(t *gatewayTurn, req apiRequest, model Model) (*Receipt, error) {
	tools := []any{}
	for _, tool := range req.Tools {
		tools = append(tools, object{"name": tool.Function.Name, "description": connectorDescription(tool), "inputSchema": jsonText(connectorSchema(tool))})
	}
	if batchMode(req) {
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
	prepared, records, err := g.c.prepareAPIMedia(t.ctx, req)
	if err != nil {
		return nil, err
	}
	body := localToolBody(renderAPIPrompt(prepared, connectorName(t.id)), model, workspace, sandbox, g.c.session.DeviceID, connector, tools)
	applyEffort(body, model, req.Effort)
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
	done := g.serveTools(ctx, channelCtx, cancel, p, nil)
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
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

type toolInbox struct {
	frames chan object
	done   <-chan struct{}
}

// serveTools opens the native tool channel of a task. Work sends a device's
// tool calls and commands over its newest open channel, whichever task they
// belong to, so every channel hands each frame to the task owning its
// sandbox, and each task handles its own frames in order without stalling
// any channel. Pending frames are handled first, once the channel is open. A
// failure of the channel or of handling cancels the task; the returned
// channel yields that error after both have stopped.
func (g *gateway) serveTools(ctx, streamCtx context.Context, cancel context.CancelFunc, p *toolProbe, pending []object) <-chan error {
	inbox := toolInbox{make(chan object, 64), ctx.Done()}
	g.inboxMu.Lock()
	if g.inboxes == nil {
		g.inboxes = map[string]toolInbox{}
	}
	g.inboxes[p.sandbox] = inbox
	g.inboxMu.Unlock()
	handled, read := make(chan error, 1), make(chan error, 1)
	go func() {
		var err error
		for err == nil && ctx.Err() == nil {
			select {
			case frame := <-inbox.frames:
				if ctx.Err() == nil {
					err = p.handle(ctx, frame)
				}
			case <-ctx.Done():
			}
		}
		if err != nil {
			cancel()
		}
		handled <- err
	}()
	go func() {
		response, err := g.c.request(streamCtx, "POST", "/alice/office/tool_local/chunk_stream", url.Values{"sand_box_id": []string{p.sandbox}, "client_version": []string{"unspecified"}}, object{"device_id": g.c.session.DeviceID, "client_attrs": object{"sandbox_id": p.sandbox}})
		if err == nil {
			if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				err = errors.New("native tool channel is not SSE")
			} else {
				for _, frame := range pending {
					g.deliver(ctx, frame)
				}
				err = consumeSSE(response.Body, func(_ string, frame object, _ string) (bool, error) { g.deliver(ctx, frame); return false, nil })
			}
			response.Body.Close()
		}
		if ctx.Err() == nil {
			if err == nil {
				err = errors.New("native tool stream ended before completion")
			}
			cancel()
		}
		read <- err
	}()
	done := make(chan error, 1)
	go func() {
		err, handleErr := <-read, <-handled
		g.inboxMu.Lock()
		delete(g.inboxes, p.sandbox)
		g.inboxMu.Unlock()
		if handleErr != nil {
			err = handleErr
		}
		done <- err
	}()
	return done
}

// deliver queues a native frame for the running task owning its sandbox.
// Frames for any other sandbox belong to a task that has ended or to another
// program using this device, and are dropped.
func (g *gateway) deliver(ctx context.Context, frame object) {
	if kind := number(frame["event_type"]); kind != 1 && kind != 2 {
		return
	}
	g.inboxMu.Lock()
	inbox, ok := g.inboxes[frameSandbox(frame)]
	g.inboxMu.Unlock()
	if ok {
		select {
		case inbox.frames <- frame:
		case <-inbox.done:
		case <-ctx.Done():
		}
	}
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
