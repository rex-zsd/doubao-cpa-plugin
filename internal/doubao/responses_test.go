package doubao

import (
	"strings"
	"testing"
)

func TestResponsesPreservesMediaAndFunctionHistory(t *testing.T) {
	payload := object{"model": "TestModel", "instructions": "system text", "parallel_tool_calls": true, "input": []any{
		object{"role": "user", "content": []any{object{"type": "input_image", "image_url": "data:image/png;base64,YQ=="}, object{"type": "input_file", "file_url": "https://example.com/a.pdf", "filename": "a.pdf"}, object{"type": "input_file", "file_id": "file_0123456789abcdef0123456789abcdef"}}},
		object{"type": "function_call", "call_id": "a", "name": "first", "arguments": `{ "x": 1 }`}, object{"type": "function_call", "call_id": "b", "name": "second", "arguments": "{}"},
		object{"type": "function_call_output", "call_id": "b", "output": "second value"}, object{"type": "function_call_output", "call_id": "a", "output": "first value"},
	}, "tools": []any{object{"type": "function", "name": "first", "parameters": object{"type": "object"}}}}
	data, err := NormalizeResponses([]byte(jsonText(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req, failure := parseAPIRequest(strings.NewReader(string(data)))
	if failure != nil {
		t.Fatal(failure)
	}
	if len(req.Messages) != 5 || len(req.Messages[2].ToolCalls) != 2 || req.Messages[2].ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatal("lost or changed function history", req)
	}
	parts := array(req.Messages[1].Content)
	if field(parts[1], "file", "file_url") != "https://example.com/a.pdf" || field(parts[2], "file", "file_id") != "file_0123456789abcdef0123456789abcdef" {
		t.Fatal("media source was lost")
	}
}
func TestResponsesRejectsOnlyServerSideState(t *testing.T) {
	for _, extra := range []object{
		{"previous_response_id": "resp_prior"},
		{"background": true},
		{"conversation": "conv_prior"},
		{"prompt": object{"id": "pmpt_prior"}},
		{"input": []any{object{"type": "item_reference", "id": "msg_prior"}}},
		{"input": []any{object{"type": "computer_call", "call_id": "c"}}},
	} {
		payload := object{"model": "TestModel", "input": "hello"}
		for k, v := range extra {
			payload[k] = v
		}
		if _, err := NormalizeResponses([]byte(jsonText(payload))); err == nil {
			t.Fatal("silently accepted", extra)
		}
	}
}

// Codex-style requests carry settings Work cannot honor, replayed reasoning
// and freeform tools; none of them may fail the request.
func TestResponsesAcceptsCodexStyleRequests(t *testing.T) {
	payload := object{"model": "TestModel", "store": false, "max_output_tokens": 50, "temperature": .1, "include": []any{"reasoning.encrypted_content"},
		"reasoning":   object{"effort": "high", "summary": "auto"},
		"text":        object{"format": object{"type": "json_schema", "name": "answer", "schema": object{"type": "object"}, "strict": true}},
		"tool_choice": object{"type": "custom", "name": "apply_patch"},
		"input": []any{
			object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "patch it"}}},
			object{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "opaque"},
			object{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": "*** Begin Patch"},
			object{"type": "custom_tool_call_output", "call_id": "c1", "output": "Done"},
		},
		"tools": []any{object{"type": "web_search"}, object{"type": "custom", "name": "apply_patch", "description": "Edit files.", "format": object{"type": "grammar", "syntax": "lark", "definition": "start: patch"}}},
	}
	data, err := NormalizeResponses([]byte(jsonText(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req, failure := parseAPIRequest(strings.NewReader(string(data)))
	if failure != nil {
		t.Fatal(failure)
	}
	if req.Effort != 5 || field(req.ResponseFormat, "type") != "json_schema" || jsonText(req.ToolChoice) != `{"function":{"name":"apply_patch"},"type":"function"}` {
		t.Fatal("settings were lost", string(data))
	}
	if len(req.Messages) != 3 || req.Messages[1].ToolCalls[0].Function.Arguments != `{"input":"*** Begin Patch"}` || req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "c1" {
		t.Fatal("history was changed", string(data))
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "apply_patch" || !strings.Contains(req.Tools[0].Function.Description, "start: patch") {
		t.Fatal("tools were not converted", string(data))
	}
}

// Gemini requests reach the plugin through the CPA translator with OpenAPI
// type names; the connector must still receive JSON Schema.
func TestGeminiStyleSchemaTypesAreLowerCased(t *testing.T) {
	schema := object{"type": "OBJECT", "properties": object{
		"type":   object{"type": "STRING", "enum": []any{"OBJECT"}},
		"tags":   object{"type": "ARRAY", "items": object{"type": "STRING"}},
		"either": object{"anyOf": []any{object{"type": "INTEGER"}, object{"type": []any{"NUMBER", "NULL"}}}},
	}}
	payload := object{"model": "TestModel", "messages": []any{object{"role": "user", "content": "hi"}}, "tools": []any{object{"type": "function", "function": object{"name": "f", "parameters": schema}}}}
	req, failure := parseAPIRequest(strings.NewReader(jsonText(payload)))
	if failure != nil {
		t.Fatal(failure)
	}
	want := `{"properties":{"either":{"anyOf":[{"type":"integer"},{"type":["number","null"]}]},"tags":{"items":{"type":"string"},"type":"array"},"type":{"enum":["OBJECT"],"type":"string"}},"type":"object"}`
	if got := jsonText(req.Tools[0].Function.Parameters); got != want {
		t.Fatal("schema types were not normalized", got)
	}
}

// Codex on Responses Lite declares namespaced tools in an additional_tools
// input item. Tools must get the chat names CPA's translator gives them, since
// CPA restores each returned call's name and namespace from that name.
func TestResponsesAcceptsCodexLiteRequests(t *testing.T) {
	long := strings.Repeat("x", 60) + "_tool"
	payload := object{"model": "TestModel", "reasoning": object{"effort": "low", "context": "all_turns"}, "text": object{"verbosity": "low"}, "client_metadata": object{"turn_id": "t"},
		"tool_choice": object{"type": "function", "name": "sleep", "namespace": "clock"},
		"tools":       []any{object{"type": "function", "name": "lookup", "parameters": object{"type": "object"}}, object{"type": "web_search"}},
		"input": []any{
			object{"type": "additional_tools", "id": "at_1", "role": "developer", "tools": []any{
				object{"type": "namespace", "name": "functions", "description": "", "tools": []any{
					object{"type": "custom", "name": "exec", "description": "Run code. This is a FREEFORM tool, so do not wrap the patch in JSON.", "format": object{"type": "grammar", "syntax": "lark", "definition": "start: SOURCE"}},
					object{"type": "function", "name": "wait", "parameters": object{"type": "object"}, "strict": false},
				}},
				object{"type": "namespace", "name": "clock", "description": "Time.", "tools": []any{object{"type": "function", "name": "sleep"}}},
				object{"type": "namespace", "name": "mcp__docs__", "tools": []any{object{"type": "function", "name": "search"}, object{"type": "function", "name": "mcp__docs__read"}}},
				object{"type": "namespace", "name": "long", "tools": []any{object{"type": "function", "name": long}}},
				object{"type": "function", "name": "lookup", "description": "second copy"},
			}},
			object{"type": "message", "role": "developer", "content": []any{object{"type": "input_text", "text": "base"}}, "internal_chat_message_metadata_passthrough": object{"content_item_kinds": []any{"model.base_instructions"}}},
			object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "run it"}}},
			object{"type": "custom_tool_call", "call_id": "c1", "namespace": "functions", "name": "exec", "input": "1+1"},
			object{"type": "custom_tool_call_output", "call_id": "c1", "output": "2"},
			object{"type": "function_call", "call_id": "c2", "name": "sleep", "arguments": "{}"},
			object{"type": "function_call_output", "call_id": "c2", "output": "ok"},
		},
	}
	data, err := NormalizeResponses([]byte(jsonText(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req, failure := parseAPIRequest(strings.NewReader(string(data)))
	if failure != nil {
		t.Fatal(failure)
	}
	names := []string{}
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	// "long__" + long is 71 characters, so CPA keeps its last 64.
	if strings.Join(names, " ") != "lookup functions__exec functions__wait clock__sleep mcp__docs__search mcp__docs__read "+strings.Repeat("x", 59)+"_tool" {
		t.Fatal("tools were not flattened like CPA", names)
	}
	if exec := req.Tools[1].Function; strings.Contains(exec.Description, "FREEFORM") || !strings.Contains(exec.Description, "start: SOURCE") || jsonText(exec.Parameters["required"]) != `["input"]` {
		t.Fatal("freeform tool was not converted", jsonText(exec))
	}
	if req.Tools[0].Function.Description != "" || jsonText(req.ToolChoice) != `{"function":{"name":"clock__sleep"},"type":"function"}` {
		t.Fatal("top-level copy or tool_choice was lost", string(data))
	}
	if len(req.Messages) != 6 || req.Messages[0].Role != "developer" || req.Messages[2].ToolCalls[0].Function.Name != "functions__exec" || req.Messages[2].ToolCalls[0].Function.Arguments != `{"input":"1+1"}` || req.Messages[4].ToolCalls[0].Function.Name != "clock__sleep" {
		t.Fatal("history was not mapped", string(data))
	}
}

func TestResponsesToolNamesAvoidCollisions(t *testing.T) {
	a, b := "a"+strings.Repeat("y", 62)+"_run", "b"+strings.Repeat("y", 62)+"_run"
	index := newResponsesToolIndex([]any{
		object{"type": "namespace", "name": "one", "tools": []any{object{"type": "function", "name": "get"}, object{"type": "function", "name": a}}},
		object{"type": "namespace", "name": "two", "tools": []any{object{"type": "function", "name": "get"}, object{"type": "function", "name": b}}},
	}, nil)
	names := []string{}
	for _, tool := range index.tools {
		names = append(names, tool.chat)
	}
	tail := strings.Repeat("y", 60) + "_run"
	if strings.Join(names, " ") != "one__get "+tail+" two__get "+cappedToolName(tail+"_1") {
		t.Fatal("capped names collided", names)
	}
	// "get" is declared twice, so a call without a namespace stays unresolved.
	if index.callName("", "get") != "get" || index.callName("two", "get") != "two__get" || index.callName("", tail) != tail || index.callName("three", "get") != "three__get" {
		t.Fatal("calls resolved to the wrong tool")
	}
}
