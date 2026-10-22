package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func fakeBatchRunner(g *gateway, starts, uploads *atomic.Int32) func(*gatewayTurn) {
	return func(turn *gatewayTurn) {
		starts.Add(1)
		g.mu.Lock()
		turn.connector = "fixture-connector"
		turn.generationPosts++
		g.mu.Unlock()
		if err := saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "1234567890123", RunID: "1234567890124"}); err != nil {
			g.finish(turn, "", apiFail(500, "fixture", err.Error()))
			return
		}
		calls := []any{}
		for i := 0; i < 3; i++ {
			calls = append(calls, object{"name": "client_nonce", "arguments": object{"challenge": fmt.Sprint(i)}})
		}
		batchName := batchToolName
		for _, route := range batchRoutes(turn.request) {
			for _, tool := range route.tools {
				if tool.Function.Name == "client_nonce" {
					batchName = route.name
				}
			}
		}
		result, err := g.forwardClientTool(turn.ctx, turn, object{"id": "native-batch", "name": "connector.call", "input": "native-input"}, object{"protocol_version": 1, "connector_id": "fixture-connector", "remote_tool_name": batchName, "raw_arguments": jsonText(object{"calls": calls})})
		if err != nil {
			g.finish(turn, "", apiFail(502, "fixture", err.Error()))
			return
		}
		uploads.Add(1)
		content := array(field(parsed(result["raw_result"]), "content"))
		if len(content) != 1 {
			g.finish(turn, "", apiFail(502, "fixture", "batch did not return one result collection"))
			return
		}
		g.finish(turn, textValue(field(content[0], "text")), nil)
	}
}

func TestBatchResultsAtomicOutOfOrderAndIdempotent(t *testing.T) {
	g, server := testGateway(t)
	var starts, uploads atomic.Int32
	g.run = fakeBatchRunner(g, &starts, &uploads)
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	code, first := postGateway(t, server, req, "batch-start")
	if code != 200 {
		t.Fatal(code, first)
	}
	assistant := returnedMessage(t, first)
	if len(assistant.ToolCalls) != 3 {
		t.Fatal("did not expose 3 calls", first)
	}
	ids := map[string]bool{}
	for _, call := range assistant.ToolCalls {
		if ids[call.ID] {
			t.Fatal("duplicate public call ID")
		}
		ids[call.ID] = true
	}
	continuation := req
	continuation.Messages = append(append([]apiMessage{}, req.Messages...), assistant)
	for i := 2; i >= 0; i-- {
		continuation.Messages = append(continuation.Messages, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[i].ID, Content: fmt.Sprintf("value-%d", i)})
	}
	missing := continuation
	missing.Messages = append([]apiMessage{}, continuation.Messages[:len(continuation.Messages)-1]...)
	if code, _ := postGateway(t, server, missing, ""); code != 400 {
		t.Fatal("partial batch accepted", code)
	}
	duplicate := continuation
	duplicate.Messages = append([]apiMessage{}, continuation.Messages...)
	duplicate.Messages[len(duplicate.Messages)-1].ToolCallID = assistant.ToolCalls[2].ID
	if code, _ := postGateway(t, server, duplicate, ""); code != 409 {
		t.Fatal("duplicate batch ID accepted", code)
	}
	foreign := continuation
	foreign.Messages = append([]apiMessage{}, continuation.Messages...)
	foreign.Messages[len(foreign.Messages)-1].ToolCallID = "unknown"
	if code, _ := postGateway(t, server, foreign, ""); code != 409 {
		t.Fatal("unknown batch ID accepted", code)
	}
	if uploads.Load() != 0 {
		t.Fatal("uploaded invalid/partial batch")
	}
	// Persistence failure must not consume the result channel or break retry.
	g.mu.Lock()
	pending := g.calls[assistant.ToolCalls[0].ID]
	path := pending.turn.statePath
	pending.turn.statePath = filepath.Join(g.dir, "missing", "state.json")
	g.mu.Unlock()
	if code, _ := postGateway(t, server, continuation, ""); code != 500 {
		t.Fatal("ignored persistence failure", code)
	}
	if uploads.Load() != 0 {
		t.Fatal("uploaded before persistence")
	}
	g.mu.Lock()
	pending.turn.statePath = path
	g.mu.Unlock()
	code, answer := postGateway(t, server, continuation, "")
	if code != 200 {
		t.Fatal(code, answer)
	}
	var value object
	if err := json.Unmarshal([]byte(textValue(returnedMessage(t, answer).Content)), &value); err != nil {
		t.Fatal(err)
	}
	rows := array(value["results"])
	for i, row := range rows {
		if field(row, "result") != fmt.Sprintf("value-%d", i) || number(field(row, "index")) != i {
			t.Fatal("misrouted result", value)
		}
	}
	if len(rows) != 3 || starts.Load() != 1 || uploads.Load() != 1 {
		t.Fatal("batch count mismatch", value, starts.Load(), uploads.Load())
	}
	_, replay := postGateway(t, server, req, "batch-start")
	if jsonText(first) != jsonText(replay) {
		t.Fatal("initial replay changed calls")
	}
	// Reordering results in an otherwise identical retry is harmless.
	reordered := continuation
	reordered.Messages = append([]apiMessage{}, continuation.Messages...)
	n := len(reordered.Messages)
	reordered.Messages[n-1], reordered.Messages[n-3] = reordered.Messages[n-3], reordered.Messages[n-1]
	code, replay = postGateway(t, server, reordered, "")
	if code != 200 || jsonText(answer) != jsonText(replay) || uploads.Load() != 1 {
		t.Fatal("duplicate upload or changed answer", code, replay)
	}
	// Different results for an answered batch start a new task over the
	// full transcript instead of failing.
	conflict := continuation
	conflict.Messages = append([]apiMessage{}, continuation.Messages...)
	conflict.Messages[len(conflict.Messages)-1].Content = "different"
	if code, replaced := postGateway(t, server, conflict, ""); code != 200 || len(returnedMessage(t, replaced).ToolCalls) != 3 || starts.Load() != 2 {
		t.Fatal("conflicting results did not start a new task", code, replaced, starts.Load())
	}
	changed := req
	parallelFalse := false
	changed.Parallel = &parallelFalse
	if code, _ := postGateway(t, server, changed, "batch-start"); code != 409 {
		t.Fatal("idempotency ignored parallel mode")
	}
}

func TestBatchStreamingExposesAllIndexedCalls(t *testing.T) {
	g, server := testGateway(t)
	var starts, uploads atomic.Int32
	g.run = fakeBatchRunner(g, &starts, &uploads)
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	req.Stream = true
	chunks := readGatewayStream(t, server, req)
	count := 0
	for _, chunk := range chunks {
		choices := array(chunk["choices"])
		if len(choices) == 0 {
			continue
		}
		for _, call := range array(field(choices[0], "delta", "tool_calls")) {
			if number(field(call, "index")) != count {
				t.Fatal("invalid SSE index", call)
			}
			count++
		}
	}
	if count != 3 {
		t.Fatal("lost streaming tool calls", count)
	}
}

func TestLargeParallelCatalogKeepsOriginalFunctionsAndSmallBatch(t *testing.T) {
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	if !batchMode(req) || compactBatchMode(req) {
		t.Fatal("small catalog changed mode")
	}
	for i := 0; i < 10; i++ {
		req.Tools = append(req.Tools, apiTool{Type: "function", Function: apiFunction{Name: fmt.Sprintf("lookup_%d", i), Description: strings.Repeat("x", 5000), Parameters: object{"type": "object", "properties": object{}}}})
	}
	if !batchMode(req) || !compactBatchMode(req) {
		t.Fatal("large catalog lost batching")
	}
	catalog := toolCatalog(req)
	if len(catalog) != len(req.Tools)+len(batchRoutes(req)) {
		t.Fatal("original functions missing")
	}
	for i, tool := range req.Tools {
		if field(catalog[i], "name") != tool.Function.Name || field(catalog[i], "description") != tool.Function.Description || field(catalog[i], "inputSchema") != jsonText(tool.Function.Parameters) {
			t.Fatal("original definition changed")
		}
	}
	if utf16Len(textValue(field(catalog[len(catalog)-1], "inputSchema"))) >= batchSchemaLimit {
		t.Fatal("batch envelope duplicates full schema")
	}
	prompt := renderAPIPrompt(req, "connector", false)
	if !strings.Contains(prompt, batchToolName) || !strings.Contains(prompt, "__client_nonce") || !strings.Contains(prompt, "__lookup_9") {
		t.Fatal("function discovery routes missing")
	}
	g, server := testGateway(t)
	var starts, uploads atomic.Int32
	g.run = fakeBatchRunner(g, &starts, &uploads)
	code, first := postGateway(t, server, req, "")
	if code != 200 || len(returnedMessage(t, first).ToolCalls) != 3 {
		t.Fatal("large batch was not exposed atomically", code)
	}
	parallel = false
	if batchMode(req) || len(toolCatalog(req)) != len(req.Tools) {
		t.Fatal("explicitly disabled parallel execution enabled")
	}
}

func TestInvalidBatchArgumentsNeverExposePartialCalls(t *testing.T) {
	g, _ := testGateway(t)
	req := toolRequest()
	turn := &gatewayTurn{request: req}
	result, err := g.forwardClientBatch(context.Background(), turn, object{"id": "n"}, object{"calls": []any{
		object{"name": "client_nonce", "arguments": object{"challenge": "valid"}},
		object{"name": "client_nonce", "arguments": object{"challenge": 42}},
	}}, req)
	if err != nil || result["isError"] != true || len(g.calls) != 0 || turn.forwarded != 0 || turn.denied != 1 {
		t.Fatal("invalid arguments escaped atomic validation", err, result)
	}
}

func TestBatchEnvelopeNeverShadowsClientFunction(t *testing.T) {
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	req.Tools[0].Function.Name = batchToolName
	if batchMode(req) || len(toolCatalog(req)) != 1 {
		t.Fatal("client function was shadowed")
	}
}
