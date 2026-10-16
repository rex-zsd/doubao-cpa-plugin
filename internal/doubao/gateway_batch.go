package doubao

import (
	"context"
	"errors"
)

const batchToolName = "client_tools_batch"

func parallelRequested(req apiRequest) bool { return req.Parallel != nil && *req.Parallel }

// The batch schema holds every function, and Work leaves a task hanging when
// one connector tool's schema is too large: batches of about 54,000 and 60,000
// characters worked, one of 60,000 and two near 100,000 hung for six minutes
// (measured 2026-10-16). A larger catalog is offered one function at a time.
const batchSchemaLimit = 48000

// batchMode reports whether the turn offers client_tools_batch instead of the
// functions themselves: the caller asked for parallel calls and they fit.
func batchMode(req apiRequest) bool {
	return parallelRequested(req) && utf16Len(batchSchema(req.Tools)) <= batchSchemaLimit
}

// This explicitly models batching. It does not fabricate parallel native calls:
// one real native connector event contains several model-selected client calls.
func batchCatalog(tools []apiTool) []any {
	return []any{object{"name": batchToolName, "description": "Execute 1 to 8 independent client functions in parallel. Include every independent lookup in ONE calls array. Wait for their real results. Submit dependent calls in a later batch. Results identify each input by zero-based index and function name.", "inputSchema": batchSchema(tools)}}
}

func batchSchema(tools []apiTool) string {
	variants := []any{}
	for _, tool := range tools {
		variants = append(variants, object{"type": "object", "description": connectorDescription(tool), "properties": object{
			"name": object{"type": "string", "enum": []string{tool.Function.Name}}, "arguments": tool.Function.Parameters,
		}, "required": []string{"name", "arguments"}, "additionalProperties": false})
	}
	return jsonText(object{"type": "object", "properties": object{"calls": object{"type": "array", "minItems": 1, "maxItems": 8, "items": object{"oneOf": variants}}}, "required": []string{"calls"}, "additionalProperties": false})
}

func (g *gateway) forwardClientBatch(ctx context.Context, t *gatewayTurn, native, args object, req apiRequest) (object, error) {
	items := array(args["calls"])
	allowed := map[string]bool{}
	for _, tool := range req.Tools {
		allowed[tool.Function.Name] = true
	}
	calls := []apiToolCall{}
	valid := len(args) == 1 && len(items) >= 1 && len(items) <= 8 && len(jsonText(args)) <= 1<<20
	for _, item := range items {
		name := textValue(field(item, "name"))
		arguments, ok := field(item, "arguments").(map[string]any)
		if !allowed[name] || !ok {
			valid = false
			break
		}
		calls = append(calls, apiToolCall{ID: "call_" + token(), Type: "function", Function: apiCallFunction{Name: name, Arguments: jsonText(arguments)}})
	}
	if !valid {
		g.mu.Lock()
		t.denied++
		denied := t.denied
		g.mu.Unlock()
		if denied > 16 {
			return nil, errors.New("too many invalid client batches")
		}
		return nativeToolError(native, "client_tools_batch requires 1–8 calls with declared function names and object arguments."), nil
	}
	if err := awaitAccepted(ctx, t.receiptPath); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if t.forwarded+len(calls) > 32 {
		g.mu.Unlock()
		return nil, errors.New("tool chain exceeded 32 client calls")
	}
	x := t.current
	message := apiMessage{Role: "assistant", Content: nil, ToolCalls: calls}
	p := &clientToolPending{turn: t, nativeID: textValue(native["id"]), exposed: x, call: calls[0], batchCalls: calls, expected: append(append([]apiMessage{}, t.request.Messages...), message), result: make(chan string, 1)}
	for _, call := range calls {
		g.calls[call.ID] = p
	}
	t.forwarded += len(calls)
	if err := g.checkpointLocked(t); err != nil {
		for _, call := range calls {
			delete(g.calls, call.ID)
		}
		t.forwarded -= len(calls)
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
	return g.toolReply(ctx, t, native, value, true)
}
