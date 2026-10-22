package main

import (
	"encoding/json"
	"strings"
)

// CPA sets SourceFormat to the selected plugin INPUT format after translating
// a Claude request. The original route/body still identify the client protocol;
// treating SourceFormat as that protocol silently loses all restored metadata.
func claudeClientRequest(req request) bool {
	if path, _ := req.Metadata["request_path"].(string); path != "" {
		return strings.HasSuffix(strings.TrimRight(path, "/"), "/messages")
	}
	if req.SourceFormat == "claude" {
		return true
	}
	if req.Headers.Get("Anthropic-Version") == "" {
		return false
	}
	// Headers alone are insufficient: clients may send Anthropic-Version on
	// OpenAI routes too. Require an unambiguous original Claude tool shape.
	var original struct {
		Tools    []map[string]json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(req.OriginalRequest, &original) != nil {
		return false
	}
	for _, tool := range original.Tools {
		if tool["input_schema"] != nil && tool["function"] == nil {
			return true
		}
	}
	for _, message := range original.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

// CPA's Claude conversion retains tool result text but drops is_error. Restore
// that bit by exact call ID and occurrence, including historical results. Raw
// fields leave schema integers and all unrelated content unchanged.
func restoreClaudeToolErrors(payload, original []byte) ([]byte, error) {
	var source struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(original, &source) != nil {
		return nil, &statusError{400, "Invalid original Claude request"}
	}
	flags := map[string][]bool{}
	for _, message := range source.Messages {
		var blocks []struct {
			Type  string          `json:"type"`
			ID    string          `json:"tool_use_id"`
			Error json.RawMessage `json:"is_error"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue // Plain text content has no tool result metadata.
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			isError := false
			if len(block.Error) > 0 && string(block.Error) != "true" && string(block.Error) != "false" {
				return nil, &statusError{400, "Claude tool_result.is_error must be a boolean"}
			}
			isError = string(block.Error) == "true"
			flags[block.ID] = append(flags[block.ID], isError)
		}
	}
	if len(flags) == 0 {
		return payload, nil
	}
	var fields map[string]json.RawMessage
	var messages []map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || json.Unmarshal(fields["messages"], &messages) != nil {
		return payload, nil // Existing request validation reports malformed JSON.
	}
	changed := false
	for _, message := range messages {
		var role, id string
		_ = json.Unmarshal(message["role"], &role)
		_ = json.Unmarshal(message["tool_call_id"], &id)
		if role != "tool" || len(flags[id]) == 0 {
			continue
		}
		isError := flags[id][0]
		flags[id] = flags[id][1:]
		if isError {
			message["is_error"] = json.RawMessage("true")
			changed = true
		}
	}
	for _, remaining := range flags {
		for _, isError := range remaining {
			if isError {
				return nil, &statusError{400, "Claude error result could not be matched to its translated tool_call_id"}
			}
		}
	}
	if !changed {
		return payload, nil
	}
	var err error
	fields["messages"], err = json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// Claude permits independent tool calls unless tool_choice explicitly disables
// them. CPA currently emits only the false case, whereas the gateway requires
// an explicit true to offer batching. Keep explicit translated restrictions.
func restoreClaudeParallel(payload, original []byte) ([]byte, error) {
	var source struct {
		Choice struct {
			Disable bool `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
	}
	if json.Unmarshal(original, &source) != nil {
		return nil, &statusError{400, "Invalid Claude parallel tool setting"}
	}
	var fields map[string]json.RawMessage
	var tools []json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || json.Unmarshal(fields["tools"], &tools) != nil || len(tools) == 0 {
		return payload, nil
	}
	if source.Choice.Disable {
		fields["parallel_tool_calls"] = json.RawMessage("false")
	} else if raw := fields["parallel_tool_calls"]; len(raw) == 0 || string(raw) == "null" {
		fields["parallel_tool_calls"] = json.RawMessage("true")
	} else {
		return payload, nil
	}
	return json.Marshal(fields)
}
