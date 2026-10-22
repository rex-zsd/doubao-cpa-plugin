package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestClaudeToolErrorsMatchExactIDAndOccurrence(t *testing.T) {
	payload := []byte(`{"model":"alias","messages":[{"role":"tool","tool_call_id":"b","content":"same"},{"role":"tool","tool_call_id":"a","content":"same"},{"role":"tool","tool_call_id":"a","content":"same"}],"tools":[{"number":9007199254740993}],"reasoning_effort":7}`)
	original := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"same","is_error":true},{"type":"tool_result","tool_use_id":"b","content":"same"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"same","is_error":false}]}]}`)
	out, err := executorPayload(request{Payload: payload, OriginalRequest: original, SourceFormat: "claude", Model: "doubao-orange-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Messages []struct {
			IsError bool `json:"is_error"`
		}
		Tools  json.RawMessage
		Effort int `json:"reasoning_effort"`
	}
	if json.Unmarshal(out, &result) != nil || len(result.Messages) != 3 || result.Messages[0].IsError || !result.Messages[1].IsError || result.Messages[2].IsError || !bytes.Contains(result.Tools, []byte("9007199254740993")) || result.Effort != 7 {
		t.Fatal("error metadata or unrelated raw fields changed", string(out))
	}
	// OpenAI inputs are not reinterpreted using an unrelated original payload.
	out, err = executorPayload(request{Payload: payload, OriginalRequest: original})
	if err != nil || !bytes.Equal(out, payload) {
		t.Fatal("non-Claude request changed", err)
	}
}

func TestClaudeUnmatchedAndInvalidErrorsFailExplicitly(t *testing.T) {
	for _, original := range []string{
		`{"messages":[{"content":[{"type":"tool_result","tool_use_id":"unknown","is_error":true}]}]}`,
		`{"messages":[{"content":[{"type":"tool_result","tool_use_id":"a","is_error":"true"}]}]}`,
		`{"messages":[{"content":[{"type":"tool_result","tool_use_id":"a","is_error":null}]}]}`,
	} {
		if _, err := executorPayload(request{Payload: []byte(`{"messages":[{"role":"tool","tool_call_id":"a","content":"x"}]}`), OriginalRequest: []byte(original), SourceFormat: "claude"}); err == nil {
			t.Fatal("ambiguous error metadata silently lost")
		}
	}
}

func TestClaudeParallelSemanticsPreserveExplicitRestrictions(t *testing.T) {
	for _, tc := range []struct {
		setting, normalized string
		want                bool
	}{
		{``, ``, true},
		{`,"tool_choice":{"type":"auto","disable_parallel_tool_use":false}`, ``, true},
		{`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`, ``, false},
		{``, `,"parallel_tool_calls":false`, false},
		{`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`, `,"parallel_tool_calls":true`, false},
	} {
		payload := []byte(`{"model":"alias","tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"n":{"default":9007199254740993}}}}}],"messages":[{"role":"user","content":"full input"}],"reasoning_effort":7` + tc.normalized + `}`)
		out, err := executorPayload(request{Payload: payload, SourceFormat: "claude", OriginalRequest: []byte(`{"messages":[]` + tc.setting + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Parallel *bool `json:"parallel_tool_calls"`
		}
		if json.Unmarshal(out, &result) != nil || result.Parallel == nil || *result.Parallel != tc.want || !bytes.Contains(out, []byte("9007199254740993")) {
			t.Fatal("parallel semantics or full schema lost", string(out))
		}
	}
}

func TestClaudeOriginalMetadataSurvivesHostFormatTranslation(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"tool","tool_call_id":"a","content":"same"}]}`)
	original := []byte(`{"tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"same","is_error":true}]}]}`)
	for _, req := range []request{
		{SourceFormat: "openai", Metadata: map[string]any{"request_path": "/v1/messages"}},
		{SourceFormat: "chat-completions", Metadata: map[string]any{"request_path": "/api/provider/x/v1/messages"}},
		{SourceFormat: "openai", Headers: http.Header{"Anthropic-Version": {"2023-06-01"}}},
	} {
		req.Payload, req.OriginalRequest = payload, original
		out, err := executorPayload(req)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Parallel *bool `json:"parallel_tool_calls"`
			Messages []struct {
				Error bool `json:"is_error"`
			}
		}
		if json.Unmarshal(out, &result) != nil || result.Parallel == nil || !*result.Parallel || !result.Messages[0].Error {
			t.Fatal("host input format hid the original Claude protocol", string(out))
		}
	}
	if claudeClientRequest(request{SourceFormat: "openai", Metadata: map[string]any{"request_path": "/v1/chat/completions"}, Headers: http.Header{"Anthropic-Version": {"2023-06-01"}}, OriginalRequest: original}) {
		t.Fatal("explicit OpenAI route reinterpreted as Claude")
	}
	if claudeClientRequest(request{SourceFormat: "openai", Headers: http.Header{"Anthropic-Version": {"2023-06-01"}}, OriginalRequest: payload}) {
		t.Fatal("a header alone reinterpreted OpenAI tool results")
	}
}
