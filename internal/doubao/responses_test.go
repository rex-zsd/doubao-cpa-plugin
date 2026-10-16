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
func TestResponsesRejectsUnimplementedSemantics(t *testing.T) {
	for _, extra := range []object{{"previous_response_id": "resp_prior"}, {"store": true}, {"max_output_tokens": 50}, {"temperature": .1}, {"reasoning": object{"effort": "low"}}} {
		payload := object{"model": "TestModel", "input": "hello"}
		for k, v := range extra {
			payload[k] = v
		}
		if _, err := NormalizeResponses([]byte(jsonText(payload))); err == nil {
			t.Fatal("silently accepted", extra)
		}
	}
}
