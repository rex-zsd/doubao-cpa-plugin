package doubao

import (
	"bytes"
	"encoding/json"
)

// NormalizeResponses keeps full history and every supported media source intact.
// Output remains Chat Completions so CPA owns the outward Responses framing.
func NormalizeResponses(payload []byte) ([]byte, error) {
	var source object
	if err := decode(bytes.NewReader(payload), &source); err != nil || source == nil {
		return nil, apiFail(400, "invalid_json", "Expected a Responses request object")
	}
	allowed := map[string]bool{"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true, "stream": true, "user": true, "store": true, "previous_response_id": true}
	for key, value := range source {
		if !allowed[key] && value != nil {
			return nil, apiFail(400, "unsupported_parameter", "Unsupported Responses parameter: "+key)
		}
	}
	if source["previous_response_id"] != nil && source["previous_response_id"] != "" {
		return nil, apiFail(400, "full_history_required", "Send the complete input history; previous_response_id is unsupported")
	}
	if source["store"] != nil && source["store"] != false {
		return nil, apiFail(400, "response_storage_unsupported", "Server-side Responses storage is unavailable; send full history")
	}
	target := object{}
	for _, key := range []string{"model", "tool_choice", "parallel_tool_calls", "stream", "user"} {
		if v, ok := source[key]; ok {
			target[key] = v
		}
	}
	messages := []any{}
	if source["instructions"] != nil {
		if _, ok := source["instructions"].(string); !ok {
			return nil, apiFail(400, "invalid_instructions", "instructions must be text")
		}
	}
	if instructions, ok := source["instructions"].(string); ok && instructions != "" {
		messages = append(messages, object{"role": "system", "content": instructions})
	}
	var items []any
	switch input := source["input"].(type) {
	case string:
		items = []any{object{"role": "user", "content": input}}
	case []any:
		items = input
	default:
		return nil, apiFail(400, "invalid_input", "input must be text or a full history array")
	}
	for _, item := range items {
		switch textValue(field(item, "type")) {
		case "function_call":
			call := object{"id": field(item, "call_id"), "type": "function", "function": object{"name": field(item, "name"), "arguments": field(item, "arguments")}}
			if len(messages) > 0 && field(messages[len(messages)-1], "role") == "assistant" {
				previous := messages[len(messages)-1].(map[string]any)
				previous["tool_calls"] = append(array(previous["tool_calls"]), call)
			} else {
				messages = append(messages, object{"role": "assistant", "content": nil, "tool_calls": []any{call}})
			}
		case "function_call_output":
			content, err := responsesContent(field(item, "output"))
			if err != nil {
				return nil, err
			}
			messages = append(messages, object{"role": "tool", "tool_call_id": field(item, "call_id"), "content": content})
		case "reasoning":
			return nil, apiFail(400, "unsupported_input", "Reasoning replay items are not supported; send visible conversation and tool history")
		case "", "message":
			content, err := responsesContent(field(item, "content"))
			if err != nil {
				return nil, err
			}
			role := textValue(field(item, "role"))
			if role == "" {
				role = "user"
			}
			messages = append(messages, object{"role": role, "content": content})
		default:
			return nil, apiFail(400, "unsupported_input", "Unsupported Responses input item type")
		}
	}
	target["messages"] = messages
	if source["tools"] != nil {
		tools, ok := source["tools"].([]any)
		if !ok {
			return nil, apiFail(400, "invalid_tools", "tools must be an array")
		}
		converted := []any{}
		for _, tool := range tools {
			if field(tool, "type") != "function" {
				return nil, apiFail(400, "unsupported_tools", "Only caller-provided function tools are supported")
			}
			fn := object{}
			for _, key := range []string{"name", "description", "parameters", "strict"} {
				if v := field(tool, key); v != nil {
					fn[key] = v
				}
			}
			converted = append(converted, object{"type": "function", "function": fn})
		}
		target["tools"] = converted
	}
	return json.Marshal(target)
}
func responsesContent(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, apiFail(400, "unsupported_content", "Content must be text or an array of supported parts")
	}
	result := []any{}
	for _, part := range parts {
		switch field(part, "type") {
		case "input_text", "output_text":
			result = append(result, object{"type": "text", "text": field(part, "text")})
		case "input_image":
			if field(part, "file_id") != nil {
				result = append(result, object{"type": "file", "file": object{"file_id": field(part, "file_id")}})
			} else {
				result = append(result, object{"type": "image_url", "image_url": object{"url": field(part, "image_url")}})
			}
		case "input_file":
			f := object{}
			for _, key := range []string{"file_id", "file_data", "file_url", "filename"} {
				if v := field(part, key); v != nil {
					f[key] = v
				}
			}
			result = append(result, object{"type": "file", "file": f})
		default:
			return nil, apiFail(400, "unsupported_content", "Unsupported Responses content part")
		}
	}
	return result, nil
}
