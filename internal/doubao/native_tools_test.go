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
	properties := object{"file_path": object{"type": "string"}, "offset": object{"type": "integer", "minimum": 0}, "limit": object{"type": "integer", "minimum": 1}}
	required := []string{"file_path"}
	switch name {
	case "Bash":
		properties, required = object{"command": object{"type": "string"}, "description": object{"type": "string"}, "timeout": object{"type": "number"}, "run_in_background": object{"type": "boolean"}, "dangerouslyDisableSandbox": object{"type": "boolean"}}, []string{"command"}
	case "Write":
		properties, required = object{"file_path": object{"type": "string"}, "content": object{"type": "string"}}, []string{"file_path", "content"}
	case "Edit":
		properties, required = object{"file_path": object{"type": "string"}, "old_string": object{"type": "string"}, "new_string": object{"type": "string"}, "replace_all": object{"type": "boolean"}}, []string{"file_path", "old_string", "new_string"}
	}
	return apiTool{Type: "function", Function: apiFunction{Name: name, Description: "Keep this complete caller contract.", Parameters: object{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
}

func TestNativeRoutingRejectsSemanticOrSchemaChanges(t *testing.T) {
	req := toolRequest()
	req.Tools = []apiTool{nativeFixtureTool("Read"), nativeFixtureTool("Bash"), nativeFixtureTool("Write"), nativeFixtureTool("Edit")}
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
		{"background", "Bash", object{"command": "printf safe", "run_in_background": true}},
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
	for _, name := range []string{"Read", "Bash", "Write", "Edit"} {
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
		for i, name := range []string{"Read", "Write", "Edit", "Bash"} {
			args := object{"file_path": "/external/fixture.txt", "offset": json.Number("2"), "limit": json.Number("3"), "thumbnail_size": "full"}
			if name == "Bash" {
				args = object{"command": "printf actual", "timeout": json.Number("1234"), "description": "Read fixture", "sandbox_permissions": "use_default"}
			} else if name == "Write" {
				args = object{"file_path": "/external/fixture.go", "content": "package 中文\n\t🥝\n"}
			} else if name == "Edit" {
				args = object{"file_path": "/external/fixture.go", "old_string": "中文", "new_string": "main", "replace_all": true}
			}
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
	req.Tools = []apiTool{nativeFixtureTool("Read"), nativeFixtureTool("Bash"), nativeFixtureTool("Write"), nativeFixtureTool("Edit")}
	for i, name := range []string{"Read", "Write", "Edit", "Bash"} {
		code, result := postGateway(t, server, req, fmt.Sprint("leg-", i))
		if code != 200 {
			t.Fatal(code, result)
		}
		message := returnedMessage(t, result)
		_, retry := postGateway(t, server, req, fmt.Sprint("leg-", i))
		if jsonText(result) != jsonText(retry) || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != name {
			t.Fatal("retry duplicated or changed native call", result, retry)
		}
		args := parsed(message.ToolCalls[0].Function.Arguments)
		if name == "Read" && (field(args, "file_path") != "/external/fixture.txt" || number(field(args, "offset")) != 2 || field(args, "thumbnail_size") != nil) || name == "Bash" && (field(args, "command") != "printf actual" || number(field(args, "timeout")) != 1234 || field(args, "sandbox_permissions") != nil) {
			t.Fatal("client arguments altered")
		}
		if name == "Write" && jsonText(args) != jsonText(object{"file_path": "/external/fixture.go", "content": "package 中文\n\t🥝\n"}) || name == "Edit" && jsonText(args) != jsonText(object{"file_path": "/external/fixture.go", "old_string": "中文", "new_string": "main", "replace_all": true}) {
			t.Fatal("mutation parameters changed at the HTTP boundary")
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
	for _, name := range []string{"Read", "Write", "Edit"} {
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
	args := object{"file_path": "/actual/fixture.go"}
	if name == "Write" {
		args["content"] = "package main\n"
	} else if name == "Edit" {
		args["old_string"], args["new_string"], args["replace_all"] = "main", "client", false
	}
	call := apiToolCall{ID: "client-call", Type: "function", Function: apiCallFunction{Name: name, Arguments: jsonText(args)}}
	if name == "Read" {
		args["thumbnail_size"] = "full"
	}
	native := object{"id": "native", "name": name, "input": jsonText(args)}
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
