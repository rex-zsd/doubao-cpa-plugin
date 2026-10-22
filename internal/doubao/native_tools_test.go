package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeFixtureTool(name string) apiTool {
	text := func() object { return object{"type": "string"} }
	flag := func() object { return object{"type": "boolean"} }
	count := func() object { return object{"type": "number"} }
	properties := object{"file_path": text(), "offset": object{"type": "integer", "minimum": 0}, "limit": object{"type": "integer", "minimum": 1}}
	required := []string{"file_path"}
	switch name {
	case "Bash":
		properties, required = object{"command": text(), "description": text(), "timeout": count(), "run_in_background": flag(), "dangerouslyDisableSandbox": flag()}, []string{"command"}
	case "Write":
		properties, required = object{"file_path": text(), "content": text()}, []string{"file_path", "content"}
	case "Edit":
		properties, required = object{"file_path": text(), "old_string": text(), "new_string": text(), "replace_all": flag()}, []string{"file_path", "old_string", "new_string"}
	case "NotebookEdit":
		properties, required = object{"notebook_path": text(), "cell_id": text(), "cell_type": object{"type": "string", "enum": []string{"code", "markdown"}}, "edit_mode": object{"type": "string", "enum": []string{"replace", "insert", "delete"}}, "new_source": text()}, []string{"notebook_path", "new_source"}
	case "Glob":
		properties, required = object{"pattern": text(), "path": text()}, []string{"pattern"}
	case "Grep":
		properties, required = object{"pattern": text(), "path": text(), "glob": text(), "type": text(), "output_mode": object{"type": "string", "enum": []string{"content", "files_with_matches", "count"}}, "-i": flag(), "-n": flag(), "-A": count(), "-B": count(), "-C": count(), "context": count(), "multiline": flag(), "head_limit": count(), "offset": count()}, []string{"pattern"}
	case "TaskOutput":
		properties, required = object{"task_id": text(), "block": flag(), "timeout": object{"type": "number", "minimum": 0, "maximum": 600000}}, []string{"task_id"}
	case "TaskStop":
		// Like the observed caller catalog: no required field, deprecated alias.
		properties, required = object{"task_id": text(), "shell_id": text()}, nil
	}
	parameters := object{"type": "object", "properties": properties, "additionalProperties": false}
	if required != nil {
		parameters["required"] = required
	}
	return apiTool{Type: "function", Function: apiFunction{Name: name, Description: "Keep this complete caller contract.", Parameters: parameters}}
}

var nativeFixtureNames = []string{"Read", "Write", "Edit", "Bash", "NotebookEdit", "Glob", "Grep", "TaskOutput", "TaskStop"}

func nativeFixtureTools(names ...string) []apiTool {
	if len(names) == 0 {
		names = nativeFixtureNames
	}
	tools := []apiTool{}
	for _, name := range names {
		tools = append(tools, nativeFixtureTool(name))
	}
	return tools
}

// nativeFixtureCall returns a compatible Work input and the exact arguments
// the HTTP caller must receive for it.
func nativeFixtureCall(name string) (native, public object) {
	switch name {
	case "Read":
		public = object{"file_path": "/external/fixture.txt", "offset": json.Number("2"), "limit": json.Number("3")}
	case "Write":
		public = object{"file_path": "/external/fixture.go", "content": "package 中文\n\t🥝\n"}
	case "Edit":
		public = object{"file_path": "/external/fixture.go", "old_string": "中文", "new_string": "main", "replace_all": true}
	case "Bash":
		public = object{"command": "sleep 1; printf actual", "timeout": json.Number("1234"), "description": "Run fixture", "run_in_background": true}
	case "NotebookEdit":
		public = object{"notebook_path": "/external/fixture.ipynb", "cell_id": "cell-1", "cell_type": "code", "edit_mode": "insert", "new_source": "print('中文')\n"}
	case "Glob":
		public = object{"pattern": "**/*.go", "path": "/external"}
	case "Grep":
		public = object{"pattern": "func (main|中文)", "path": "/external", "glob": "*.go", "output_mode": "content", "-n": true, "-C": json.Number("2"), "head_limit": json.Number("20"), "multiline": false}
	case "TaskOutput":
		public = object{"task_id": "bg-1", "block": true, "timeout": json.Number("5000")}
	case "TaskStop":
		public = object{"shell_id": "bg-1"}
	}
	native = object{}
	for key, value := range public {
		native[key] = value
	}
	if name == "Read" {
		native["thumbnail_size"] = "full"
	} else if name == "Bash" {
		native["sandbox_permissions"] = "use_default"
	}
	return native, public
}

func TestNativeRoutingRejectsSemanticOrSchemaChanges(t *testing.T) {
	req := toolRequest()
	req.Tools = nativeFixtureTools()
	for _, tc := range []struct {
		name string
		tool string
		args object
	}{
		{"alias is not translated", "Read", object{"path": "/fixture"}},
		{"relative path", "Read", object{"file_path": "fixture"}},
		{"zero origin", "Read", object{"file_path": "/fixture", "offset": 0}},
		{"fractional offset", "Read", object{"file_path": "/fixture", "offset": 1.5}},
		{"unknown field", "Read", object{"file_path": "/fixture", "unsupported": true}},
		{"PDF route", "Read", object{"file_path": "/fixture.pdf", "pages": "1"}},
		{"image preview", "Read", object{"file_path": "/fixture.png", "thumbnail_size": "full"}},
		{"changed preview", "Read", object{"file_path": "/fixture.txt", "thumbnail_size": "small"}},
		{"missing command", "Bash", object{}},
		{"elevation", "Bash", object{"command": "printf safe", "dangerouslyDisableSandbox": true}},
		{"native elevation", "Bash", object{"command": "printf safe", "sandbox_permissions": "require_escalated"}},
		{"timeout type", "Bash", object{"command": "printf safe", "timeout": "1000"}},
		{"undeclared function", "Delete", object{"file_path": "/fixture"}},
		{"unknown Read format", "Read", object{"file_path": "/fixture.unknown"}},
		{"missing write content", "Write", object{"file_path": "/fixture.go"}},
		{"relative Write", "Write", object{"file_path": "fixture.go", "content": "x"}},
		{"write encoding option", "Write", object{"file_path": "/fixture.go", "content": "x", "encoding": "base64"}},
		{"edit old text absent", "Edit", object{"file_path": "/fixture.go", "new_string": "x"}},
		{"empty edit target", "Edit", object{"file_path": "/fixture.go", "old_string": "", "new_string": "x"}},
		{"edit replace_all type", "Edit", object{"file_path": "/fixture.go", "old_string": "x", "new_string": "y", "replace_all": "true"}},
		{"background type", "Bash", object{"command": "printf safe", "run_in_background": "true"}},
		{"relative notebook", "NotebookEdit", object{"notebook_path": "fixture.ipynb", "new_source": "x"}},
		{"notebook mode outside schema", "NotebookEdit", object{"notebook_path": "/fixture.ipynb", "new_source": "x", "edit_mode": "move"}},
		{"notebook source missing", "NotebookEdit", object{"notebook_path": "/fixture.ipynb", "cell_id": "c", "edit_mode": "delete"}},
		{"empty Glob pattern", "Glob", object{"pattern": ""}},
		{"relative Glob path", "Glob", object{"pattern": "*.go", "path": "src"}},
		{"Glob ordering", "Glob", object{"pattern": "*.go", "sort_order": "asc"}},
		{"Glob entry kinds", "Glob", object{"pattern": "*", "include_directories": true}},
		{"Glob result limit", "Glob", object{"pattern": "*", "max_results": 5}},
		{"Grep include alias", "Grep", object{"pattern": "x", "include": "*.go"}},
		{"relative Grep path", "Grep", object{"pattern": "x", "path": "."}},
		{"Grep mode outside schema", "Grep", object{"pattern": "x", "output_mode": "lines"}},
		{"Grep flag type", "Grep", object{"pattern": "x", "-i": "yes"}},
		{"TaskOutput shell alias", "TaskOutput", object{"shell_id": "bg-1"}},
		{"TaskOutput block type", "TaskOutput", object{"task_id": "bg-1", "block": "true"}},
		{"TaskOutput timeout limit", "TaskOutput", object{"task_id": "bg-1", "timeout": 900000}},
		{"missing task", "TaskStop", object{}},
		{"empty task", "TaskStop", object{"task_id": ""}},
		{"numeric task", "TaskStop", object{"task_id": 7}},
		{"empty alias beside task", "TaskStop", object{"task_id": "bg-1", "shell_id": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := nativeClientCall(req, object{"id": "n", "name": tc.tool}, tc.args); ok {
				t.Fatal("silently changed the caller contract")
			}
			g, _ := testGateway(t)
			g.nativeTools = true
			turn := &gatewayTurn{id: "0123456789abcdef0123456789abcdef", request: req, nativeTools: true}
			result, err := g.forwardClientTool(context.Background(), turn, object{"id": "n", "name": tc.tool, "input": jsonText(tc.args)}, tc.args)
			if err != nil || result["isError"] != true || turn.forwarded != 0 {
				t.Fatal("incompatible native tool was forwarded", err, result)
			}
		})
	}
	for _, name := range nativeFixtureNames {
		tool := nativeFixtureTool(name)
		if !nativeCompatibleTool(tool) {
			t.Fatal("known fixture schema rejected")
		}
		tool.Function.Parameters["required"] = []string{"custom_semantics"}
		if nativeCompatibleTool(tool) {
			t.Fatal("same tool name with a different required contract accepted")
		}
	}
}

func TestNativeTextAndMutationArgumentsArePreserved(t *testing.T) {
	req := toolRequest()
	req.Tools = []apiTool{nativeFixtureTool("Read"), nativeFixtureTool("Write"), nativeFixtureTool("Edit")}
	for _, file := range []string{"file.go", "file.py", "file.js", "file.ts", "file.md", "file.json", "file.yaml", "file.sh", "file.TSX", "Dockerfile", "Makefile", ".gitignore"} {
		args := object{"file_path": "/client/" + file, "offset": 1, "limit": 3}
		name, got, ok := nativeClientCall(req, object{"id": "native", "name": "Read"}, args)
		if !ok || name != "Read" || jsonText(got) != jsonText(args) {
			t.Fatal("known text Read changed or rejected", file)
		}
	}
	for _, tc := range []struct {
		name string
		args object
	}{
		{"Write", object{"file_path": "/client/a.go", "content": "中文\r\n\t🥝\\n\n"}},
		{"Write", object{"file_path": "/client/empty", "content": ""}},
		{"Edit", object{"file_path": "/client/a.py", "old_string": "中文\r\n", "new_string": "", "replace_all": false}},
		{"Edit", object{"file_path": "/client/a.json", "old_string": "old", "new_string": "new", "replace_all": true}},
	} {
		name, got, ok := nativeClientCall(req, object{"id": "native", "name": tc.name}, tc.args)
		if !ok || name != tc.name || jsonText(got) != jsonText(tc.args) {
			t.Fatal("mutation arguments were normalized or lost", tc.name)
		}
	}
	req.Tools = []apiTool{nativeFixtureTool("Read")}
	if _, _, ok := nativeClientCall(req, object{"id": "native", "name": "Write"}, object{"file_path": "/client/a.go", "content": "x"}); ok {
		t.Fatal("undeclared native Write was exposed")
	}
}

func TestNativeSearchNotebookTaskAndBackgroundCallsArePreserved(t *testing.T) {
	req := toolRequest()
	req.Tools = nativeFixtureTools()
	for _, tc := range []struct {
		name string
		args object
	}{
		{"Glob", object{"pattern": "**/*.go"}},
		{"Glob", object{"pattern": "*.md", "path": "/client/docs"}},
		{"Grep", object{"pattern": "a\\s+b", "path": "/client", "glob": "*.go", "output_mode": "content", "-n": true, "-C": 2, "head_limit": 20, "offset": 5}},
		{"Grep", object{"pattern": "TODO", "type": "go", "-i": true, "-A": 1, "-B": 0, "context": 3, "multiline": true}},
		{"NotebookEdit", object{"notebook_path": "/client/a.ipynb", "cell_id": "c1", "new_source": "x = 1\r\n中文", "edit_mode": "replace", "cell_type": "markdown"}},
		{"NotebookEdit", object{"notebook_path": "/client/a.ipynb", "cell_id": "c1", "new_source": "", "edit_mode": "delete"}},
		{"Bash", object{"command": "sleep 30; echo done", "run_in_background": true, "description": "background", "timeout": 600000}},
		{"TaskOutput", object{"task_id": "bg-1", "block": false, "timeout": 0}},
		{"TaskStop", object{"task_id": "bg-1"}},
		{"TaskStop", object{"shell_id": "bg-1"}},
	} {
		name, got, ok := nativeClientCall(req, object{"id": "native", "name": tc.name}, tc.args)
		if !ok || name != tc.name || jsonText(got) != jsonText(tc.args) {
			t.Fatal("native call changed or rejected", tc.name, tc.args)
		}
	}
	// Background runs are the caller's feature, never assumed from the name.
	bash := nativeFixtureTool("Bash")
	delete(bash.Function.Parameters["properties"].(object), "run_in_background")
	req.Tools = []apiTool{bash}
	if _, _, ok := nativeClientCall(req, object{"id": "native", "name": "Bash"}, object{"command": "sleep 30", "run_in_background": true}); ok {
		t.Fatal("background Bash forwarded to a caller without background runs")
	}
	// Like the observed Claude Code catalog: no Glob, Grep or TaskOutput.
	req.Tools = nativeFixtureTools("Read", "Bash", "NotebookEdit", "TaskStop")
	docs := nativeClientInstructions(req, "c")
	for name, args := range map[string]object{"Glob": {"pattern": "*.go"}, "Grep": {"pattern": "x"}, "TaskOutput": {"task_id": "bg-1"}} {
		if _, _, ok := nativeClientCall(req, object{"id": "native", "name": name}, args); ok || strings.Contains(docs, name+"\nKeep this complete caller contract.") {
			t.Fatal("undeclared native tool offered or forwarded", name)
		}
		g, _ := testGateway(t)
		turn := &gatewayTurn{id: "0123456789abcdef0123456789abcdef", request: req, nativeTools: true}
		result, err := g.forwardClientTool(context.Background(), turn, object{"id": "n", "name": name, "input": jsonText(args)}, args)
		if err != nil || result["isError"] != true || result["error_code"] != "CLIENT_TOOL_ONLY" || turn.forwarded != 0 || !strings.Contains(textValue(result["error_msg"]), "tool_search") {
			t.Fatal("undeclared native tool lost its MCP route", name, err, result)
		}
	}
	for _, name := range []string{"Read", "Bash", "NotebookEdit", "TaskStop"} {
		if !strings.Contains(docs, name+"\nKeep this complete caller contract.") {
			t.Fatal("declared native tool missing from the prompt", name)
		}
	}
}

func TestNativeRoutingRejectsSandboxEscapeOption(t *testing.T) {
	req := toolRequest()
	req.Tools = nativeFixtureTools("Bash")
	args := object{"command": "printf safe", "sandbox_permissions": "use_default"}
	for _, tc := range []struct {
		option any
		ok     bool
	}{
		{nil, true},
		{jsonText(object{"agent_type": "main_agent"}), true},
		{jsonText(object{"bash_sandbox_escape": ""}), true},
		{jsonText(object{"bash_sandbox_escape": object{"sandbox_permissions": "use_default"}}), true},
		{jsonText(object{"bash_sandbox_escape": jsonText(object{"sandbox_permissions": "use_default", "justification": ""})}), true},
		{jsonText(object{"bash_sandbox_escape": object{"sandbox_permissions": "require_escalated", "justification": "write outside"}}), false},
		{jsonText(object{"bash_sandbox_escape": jsonText(object{"sandbox_permissions": "require_escalated"})}), false},
		{jsonText(object{"bash_sandbox_escape": "unparsed"}), false},
		{object{"bash_sandbox_escape": object{"sandbox_permissions": "require_escalated"}}, false},
	} {
		call := object{"id": "native", "name": "Bash", "option": tc.option}
		if _, _, ok := nativeClientCall(req, call, args); ok != tc.ok {
			t.Fatal("sandbox escape routing changed", tc.option)
		}
	}
}

func TestNativeRoutingPreservesRealResultsErrorsAndHTTPIdempotency(t *testing.T) {
	g, server := testGateway(t)
	g.nativeTools = true
	var generations atomic.Int32
	g.run = func(turn *gatewayTurn) {
		generations.Add(1)
		if err := saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "fixture-conversation", RunID: "fixture-run"}); err != nil {
			g.finish(turn, "", apiFail(500, "fixture", err.Error()))
			return
		}
		for i, name := range nativeFixtureNames {
			args, _ := nativeFixtureCall(name)
			call := object{"id": fmt.Sprint("native-", i), "name": name, "input": jsonText(args)}
			reply, err := g.forwardClientTool(turn.ctx, turn, call, args)
			if err != nil || reply["id"] != call["id"] || reply["name"] != name || reply["tool_input"] != call["input"] || reply["raw_result"] != nil || reply["isError"] != (i%2 == 1) || textValue(field(array(reply["content"])[0], "text")) != fmt.Sprint("ACTUAL_", i) {
				t.Error("native identity, result or error lost", reply, err)
				g.finish(turn, "bad fixture", nil)
				return
			}
		}
		g.finish(turn, "verified", nil)
	}
	req := toolRequest()
	req.Tools = nativeFixtureTools()
	for i, name := range nativeFixtureNames {
		code, result := postGateway(t, server, req, fmt.Sprint("leg-", i))
		if code != 200 {
			t.Fatal(code, result)
		}
		message := returnedMessage(t, result)
		_, retry := postGateway(t, server, req, fmt.Sprint("leg-", i))
		if jsonText(result) != jsonText(retry) || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != name {
			t.Fatal("retry duplicated or changed native call", result, retry)
		}
		// Only the observed Read/Bash platform defaults may be absent.
		if _, public := nativeFixtureCall(name); jsonText(parsed(message.ToolCalls[0].Function.Arguments)) != jsonText(public) {
			t.Fatal("client arguments altered at the HTTP boundary", name)
		}
		req.Messages = append(req.Messages, message, apiMessage{Role: "tool", ToolCallID: message.ToolCalls[0].ID, Content: fmt.Sprint("ACTUAL_", i), IsError: i%2 == 1})
	}
	code, final := postGateway(t, server, req, "final")
	if code != 200 || returnedMessage(t, final).Content != "verified" || generations.Load() != 1 {
		t.Fatal("native tools restarted generation", final, generations.Load())
	}
}

func TestNativeRoutingPromptRetainsOriginalCatalogAndDefault(t *testing.T) {
	req := toolRequest()
	req.Tools = append(req.Tools, nativeFixtureTool("Read"), nativeFixtureTool("Bash"))
	base := renderAPIPrompt(req, "connector", false)
	probe := renderAPIRoutedPrompt(req, "connector", false, true)
	if !strings.Contains(base, "不要改用你所在平台自带的 Bash") || strings.Contains(base, "原生工具转发：") {
		t.Fatal("default routing changed")
	}
	if !strings.Contains(probe, "原生工具转发：") || !strings.Contains(probe, req.Tools[1].Function.Description) || !strings.Contains(probe, plainJSON(req.Tools[1].Function.Parameters)) || !strings.Contains(probe, "__client_nonce") || len(toolCatalog(req)) != 3 {
		t.Fatal("native experiment lost original tools, descriptions or schema")
	}
}

func TestNativeRoutingKeepsBatchPriorityAndForcedToolChoice(t *testing.T) {
	req := toolRequest()
	req.Tools = []apiTool{nativeFixtureTool("Read"), nativeFixtureTool("Bash")}
	req.Parallel = new(bool)
	*req.Parallel = true
	prompt := renderAPIRoutedPrompt(req, "connector", false, true)
	if !strings.Contains(prompt, "多个独立调用必须优先使用上面的 MCP 并行批入口") || !strings.Contains(prompt, "__client_tools_batch") {
		t.Fatal("native route replaced independent batching")
	}
	req.ToolChoice = object{"type": "function", "function": object{"name": "Bash"}}
	if _, _, ok := nativeClientCall(req, object{"id": "n", "name": "Read"}, object{"file_path": "/fixture.txt"}); ok {
		t.Fatal("native Read bypassed forced Bash")
	}
	if strings.Contains(nativeClientInstructions(req, "c"), "Read\nKeep this complete caller contract.") {
		t.Fatal("prompt offered a native tool excluded by tool_choice")
	}
}

func TestNativeRoutingFallsBackWithoutTruncatingValidLongPrompt(t *testing.T) {
	g, _ := testGateway(t)
	req := toolRequest()
	req.Tools = []apiTool{nativeFixtureTool("Read")}
	req.Messages = []apiMessage{{Role: "user", Content: ""}}
	base := utf16Len(renderAPIPrompt(req, "connector", false))
	req.Messages[0].Content = strings.Repeat("x", maxPromptChars-base-20)
	want := renderAPIPrompt(req, "connector", false)
	turn := &gatewayTurn{id: token(), request: req, nativeTools: true, statePath: filepath.Join(g.dir, "fallback.json")}
	prompt, err := g.renderToolPrompt(turn, req, "connector", false)
	if err != nil || prompt != want || turn.nativeTools || utf16Len(prompt) != maxPromptChars-20 {
		t.Fatal("native documentation invalidated or truncated an otherwise valid request", err)
	}
}

func TestNativeRoutingRecoveryKeepsPendingCallProtocolWithConfigDisabled(t *testing.T) {
	for _, name := range nativeFixtureNames {
		t.Run(name, func(t *testing.T) { testNativeRoutingRecovery(t, name) })
	}
}

func testNativeRoutingRecovery(t *testing.T, name string) {
	c, _ := newClient(t.TempDir())
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, false)
	turn.request.Tools = []apiTool{nativeFixtureTool(name)}
	turn.initialHash = apiFingerprint(turn.request)
	turn.nativeTools, turn.connector, turn.sandbox = true, "connector", "sandbox"
	input, args := nativeFixtureCall(name)
	call := apiToolCall{ID: "client-call", Type: "function", Function: apiCallFunction{Name: name, Arguments: jsonText(args)}}
	native := object{"id": "native", "name": name, "input": jsonText(input)}
	assistant := apiMessage{Role: "assistant", ToolCalls: []apiToolCall{call}}
	pending := &clientToolPending{turn: turn, nativeID: "native", exposed: turn.first, call: call, expected: append(append([]apiMessage{}, turn.request.Messages...), assistant), result: make(chan string, 1)}
	g.calls[call.ID] = pending
	turn.forwarded = 1
	turn.first.resolve(assistant, "tool_calls", nil)
	frame := object{"event_type": 1, "sandbox_id": "sandbox", "tool_call": native}
	_ = saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "conv", RunID: "run"})
	_ = saveJSON(filepath.Join(c.dir, "gateway-native-"+turn.id+".json"), object{"1:native": object{"uploaded": false, "frame": frame}})
	if err := g.checkpointLocked(turn); err != nil {
		t.Fatal(err)
	}
	var uploads, generations atomic.Int32
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		status := object{"code": 0}
		header := http.Header{}
		var body io.ReadCloser
		switch r.URL.Path {
		case "/chat/completion":
			generations.Add(1)
			return nil, context.Canceled
		case "/alice/office/tool_local/chunk_stream":
			header.Set("Content-Type", "text/event-stream")
			body = io.NopCloser(io.MultiReader(strings.NewReader("data: "+jsonText(frame)+"\n\n"), untilCancelled{r.Context()}))
		case "/alice/office/tool_local/upload_tool_call_result":
			var uploaded object
			if err := decode(r.Body, &uploaded); err != nil {
				t.Error(err)
			}
			v, _ := uploaded["tool_result"].(map[string]any)
			if v["id"] != "native" || v["name"] != name || v["tool_input"] != native["input"] || v["isError"] != true || v["raw_result"] != nil || textValue(field(array(v["content"])[0], "text")) != "ACTUAL_FAILURE" {
				t.Error("recovery changed native identity, result or error", v)
			}
			uploads.Add(1)
		case "/alice/office/skills/manage/connector/list_by_user":
			status = object{"code": 0, "data": object{"items": []any{}, "has_more": false}}
		case "/alice/office/skills/manage/connector/disconnect":
		case "/im/conversation/batch_get":
			messages := []any{object{"user_type": 1, "message_id": "run"}}
			if uploads.Load() > 0 {
				messages = append(messages, object{"user_type": 2, "message_id": "reply", "bot_reply_message_id": "run", "status": 0, "content_block": []any{object{"content": object{"text_block": object{"text": "recovered actual failure"}}}}, "ext": object{"is_finish": "1"}})
			}
			status = object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": "conv", "messages": messages}}}}}
		default:
			t.Errorf("unexpected recovery route %s", r.URL.Path)
		}
		if body == nil {
			body = io.NopCloser(strings.NewReader(jsonText(status)))
		}
		return &http.Response{StatusCode: 200, Header: header, Body: body, Request: r}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := newGateway(ctx, c, nil, c.dir, time.Minute)
	// The new service intentionally has native_tools=false.
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	if !restored.turns[turn.id].nativeTools || restored.nativeTools {
		t.Fatal("per-task policy was not restored")
	}
	replay, failure := restored.submit(turn.request, "restart")
	if failure != nil || jsonText(replay.message) != jsonText(assistant) {
		t.Fatal("recovery changed public call", failure)
	}
	continuation := turn.request
	continuation.Messages = append(append([]apiMessage{}, pending.expected...), apiMessage{Role: "tool", ToolCallID: call.ID, Content: "ACTUAL_FAILURE", IsError: true})
	x, failure := restored.submit(continuation, "")
	if failure != nil {
		t.Fatal(failure)
	}
	select {
	case <-x.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered native call stalled")
	}
	restored.wg.Wait()
	turn.cancel()
	if x.err != nil || x.message.Content != "recovered actual failure" || uploads.Load() != 1 || generations.Load() != 0 {
		t.Fatal("recovery duplicated or lost native execution", x.err)
	}
}
