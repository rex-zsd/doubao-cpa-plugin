package doubao

import (
	"bytes"
	"encoding/json"
	"strings"
)

// NormalizeResponses keeps full history and every supported media source intact.
// Output remains Chat Completions so CPA owns the outward Responses framing.
// Settings Work cannot honor are ignored, like their Chat counterparts; only
// server-side state is refused, since ignoring it would silently drop history.
func NormalizeResponses(payload []byte) ([]byte, error) {
	var source object
	if err := decode(bytes.NewReader(payload), &source); err != nil || source == nil {
		return nil, apiFail(400, "invalid_json", "Expected a Responses request object")
	}
	if id := source["previous_response_id"]; id != nil && id != "" {
		return nil, apiFail(400, "full_history_required", "Send the complete input history; previous_response_id is unsupported")
	}
	if source["conversation"] != nil || source["prompt"] != nil {
		return nil, apiFail(400, "full_history_required", "Stored conversations and prompts are unsupported; send the complete input history")
	}
	if truthy(source["background"]) {
		return nil, apiFail(400, "unsupported_parameter", "Background responses are unsupported")
	}
	target := object{}
	for _, key := range []string{"model", "tool_choice", "parallel_tool_calls", "stream", "user"} {
		if v, ok := source[key]; ok {
			target[key] = v
		}
	}
	if effort := field(source, "reasoning", "effort"); effort != nil {
		target["reasoning_effort"] = effort
	}
	switch format := field(source, "text", "format"); field(format, "type") {
	case "json_object":
		target["response_format"] = object{"type": "json_object"}
	case "json_schema":
		target["response_format"] = object{"type": "json_schema", "json_schema": object{"name": field(format, "name"), "schema": field(format, "schema"), "strict": field(format, "strict")}}
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
	tools, ok := source["tools"].([]any)
	if source["tools"] != nil && !ok {
		return nil, apiFail(400, "invalid_tools", "tools must be an array")
	}
	index := newResponsesToolIndex(tools, items)
	if choice, ok := source["tool_choice"].(map[string]any); ok && (choice["type"] == "function" || choice["type"] == "custom") {
		name, namespace := "", ""
		for _, path := range [][]string{{"function", "name"}, {"custom", "name"}, {"name"}} {
			if name = textValue(field(choice, path...)); name != "" {
				break
			}
		}
		for _, path := range [][]string{{"namespace"}, {"function", "namespace"}, {"custom", "namespace"}} {
			if namespace = strings.TrimSpace(textValue(field(choice, path...))); namespace != "" {
				break
			}
		}
		if name != "" {
			target["tool_choice"] = object{"type": "function", "function": object{"name": index.callName(namespace, name)}}
		}
	}
	for _, item := range items {
		kind := textValue(field(item, "type"))
		switch kind {
		case "additional_tools":
			// Responses Lite declares tools in the input; they join the tools below.
		case "function_call", "custom_tool_call":
			arguments := field(item, "arguments")
			if kind == "custom_tool_call" {
				arguments = jsonText(object{"input": field(item, "input")})
			}
			name := index.callName(textValue(field(item, "namespace")), textValue(field(item, "name")))
			call := object{"id": field(item, "call_id"), "type": "function", "function": object{"name": name, "arguments": arguments}}
			if len(messages) > 0 && field(messages[len(messages)-1], "role") == "assistant" {
				previous := messages[len(messages)-1].(map[string]any)
				previous["tool_calls"] = append(array(previous["tool_calls"]), call)
			} else {
				messages = append(messages, object{"role": "assistant", "content": nil, "tool_calls": []any{call}})
			}
		case "function_call_output", "custom_tool_call_output":
			content, err := responsesContent(field(item, "output"))
			if err != nil {
				return nil, err
			}
			messages = append(messages, object{"role": "tool", "tool_call_id": field(item, "call_id"), "content": content})
		case "reasoning":
			// Work exposes no reasoning, so replayed items carry nothing to keep.
		case "web_search_call", "file_search_call", "image_generation_call", "code_interpreter_call", "mcp_call", "mcp_list_tools", "local_shell_call", "local_shell_call_output", "tool_search_call", "tool_search_output":
			// The caller's own hosted tools ran upstream before this request
			// (e.g. Codex web search in a thread that switched model). Work
			// cannot replay them; it keeps a short note of what they did.
			messages = append(messages, object{"role": "assistant", "content": hostedToolNote(kind, item)})
		case "item_reference":
			return nil, apiFail(400, "full_history_required", "Stored item references are unsupported; send the items themselves")
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
			return nil, apiFail(400, "unsupported_input", "Unsupported Responses input item type: "+kind)
		}
	}
	target["messages"] = messages
	converted := []any{}
	for _, tool := range index.tools {
		converted = append(converted, object{"type": "function", "function": tool.function()})
	}
	if len(converted) > 0 {
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
		switch kind := textValue(field(part, "type")); kind {
		case "input_text", "output_text", "text":
			result = append(result, object{"type": "text", "text": field(part, "text")})
		case "refusal":
			result = append(result, object{"type": "text", "text": field(part, "refusal")})
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
			return nil, apiFail(400, "unsupported_content", "Unsupported Responses content part: "+kind)
		}
	}
	return result, nil
}

// hostedToolNote describes a hosted tool record without its bulky payloads
// (images, file contents, screenshots), capped at hostedToolNoteChars.
func hostedToolNote(kind string, item any) string {
	note := object{}
	for _, name := range []string{"status", "action", "queries", "server_label", "name", "arguments", "code", "error"} {
		if value := field(item, name); value != nil {
			note[name] = value
		}
	}
	if kind == "local_shell_call_output" || kind == "tool_search_output" {
		if output := textValue(field(item, "output")); output != "" {
			note["output"] = output
		}
	}
	text := plainJSON(note)
	if runes := []rune(text); len(runes) > hostedToolNoteChars {
		text = string(runes[:hostedToolNoteChars]) + "…（已截断）"
	}
	return "（此前调用方使用了自带的托管工具 " + kind + "，记录如下（图片、文件等大块内容未随历史提供）：" + text
}

const hostedToolNoteChars = 4000
