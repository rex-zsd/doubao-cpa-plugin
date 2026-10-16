package doubao

import (
	"context"
	"errors"
)

const batchToolName = "client_tools_batch"

func parallelRequested(req apiRequest) bool { return req.Parallel != nil && *req.Parallel }

// This explicitly models batching. It does not fabricate parallel native calls:
// one real native connector event contains several model-selected client calls.
func batchCatalog(tools []apiTool) []any {
	variants := []any{}
	for _, tool := range tools {
		variants = append(variants, object{"type": "object", "description": tool.Function.Description, "properties": object{
			"name": object{"type": "string", "enum": []string{tool.Function.Name}}, "arguments": tool.Function.Parameters,
		}, "required": []string{"name", "arguments"}, "additionalProperties": false})
	}
	schema := object{"type": "object", "properties": object{"calls": object{"type": "array", "minItems": 1, "maxItems": 8, "items": object{"oneOf": variants}}}, "required": []string{"calls"}, "additionalProperties": false}
	return []any{object{"name": batchToolName, "description": "Execute 1 to 8 independent client functions in parallel. Include every independent lookup in ONE calls array. Wait for their real results. Submit dependent calls in a later batch. Results identify each input by zero-based index and function name.", "inputSchema": jsonText(schema)}}
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

// The first implementation accepts a complete result batch atomically, in any
// order. It never uploads a partial batch or starts another native generation.
// Caller holds g.mu, matching the existing scalar continuation path.
func (g *gateway) submitBatchLocked(req apiRequest, p *clientToolPending) (*apiExchange, *apiError) {
	n := len(p.expected)
	if len(req.Messages) < n || !apiMessagesEqual(req.Messages[:n], p.expected) {
		return nil, apiFail(409, "tool_history_mismatch", "Batch continuation requires the exact assistant message and prior history")
	}
	if len(req.Messages) != n+len(p.batchCalls) {
		return nil, apiFail(400, "incomplete_tool_batch", "Supply one tool result for every call in this batch in one request")
	}
	wanted, values := map[string]bool{}, map[string]string{}
	for _, call := range p.batchCalls {
		wanted[call.ID] = true
	}
	for _, message := range req.Messages[n:] {
		_, duplicate := values[message.ToolCallID]
		if message.Role != "tool" || !wanted[message.ToolCallID] || duplicate {
			return nil, apiFail(409, "invalid_tool_batch", "Batch contains unknown, duplicate or non-tool results")
		}
		values[message.ToolCallID] = apiResultText(message.Content)
	}
	rows := []any{}
	for i, call := range p.batchCalls {
		rows = append(rows, object{"index": i, "name": call.Function.Name, "result": values[call.ID]})
	}
	value := jsonText(object{"results": rows})
	if p.submitted {
		if value != p.value {
			return nil, apiFail(409, "tool_result_conflict", "Different results already supplied for this batch")
		}
		return p.next, nil
	}
	t := p.turn
	if t.done {
		if t.err != nil {
			return nil, t.err
		}
		return nil, apiFail(409, "task_finished", "The upstream task has already ended")
	}
	x := newExchange(req.Model)
	previousExchange, previousRequest := t.current, t.request
	p.submitted, p.value, p.next = true, value, x
	t.current = x
	req.Tools, req.ToolChoice, req.Parallel = t.request.Tools, t.request.ToolChoice, t.request.Parallel
	t.request = req
	if err := g.checkpointLocked(t); err != nil {
		p.submitted, p.value, p.next = false, "", nil
		t.current, t.request = previousExchange, previousRequest
		return nil, apiFail(500, "state_persistence_failed", "Could not persist tool batch; nothing was uploaded")
	}
	p.result <- value
	return x, nil
}
