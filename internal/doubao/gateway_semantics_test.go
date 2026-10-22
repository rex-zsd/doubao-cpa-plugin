package doubao

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolFailureSurvivesUploadRetryAndRecovery(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	uploaded := make(chan object, 2)
	g.run = func(turn *gatewayTurn) {
		starts.Add(1)
		g.mu.Lock()
		turn.connector = "fixture"
		turn.generationPosts++
		g.mu.Unlock()
		_ = saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "1234567890123", RunID: "1234567890124"})
		result, err := g.forwardClientTool(turn.ctx, turn, object{"id": "native", "name": "connector.call"}, object{"protocol_version": 1, "connector_id": "fixture", "remote_tool_name": "client_nonce", "raw_arguments": `{"challenge":"unit"}`})
		if err != nil {
			g.finish(turn, "", apiFail(500, "fixture", err.Error()))
			return
		}
		uploaded <- parsed(result["raw_result"]).(map[string]any)
		g.finish(turn, "done", nil)
	}
	req := toolRequest()
	code, first := postGateway(t, server, req, "")
	if code != 200 {
		t.Fatal(code, first)
	}
	assistant := returnedMessage(t, first)
	req.Messages = append(req.Messages, assistant, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: "identical body", IsError: true})
	code, _ = postGateway(t, server, req, "")
	if code != 200 {
		t.Fatal(code)
	}
	if result := <-uploaded; result["isError"] != true || field(array(result["content"])[0], "text") != "identical body" {
		t.Fatal("error flag or body lost", result)
	}
	code, _ = postGateway(t, server, req, "")
	if code != 200 || starts.Load() != 1 {
		t.Fatal("exact retry reran a task", code, starts.Load())
	}
	// Completed-state recovery must retain the flag for replay and uploads.
	g.wg.Wait()
	restored := newGateway(context.Background(), g.c, g.models, g.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	for _, p := range restored.calls {
		if !p.isError {
			t.Fatal("persisted failure restored as success")
		}
	}
	x, failure := restored.submit(req, "")
	if failure != nil || x.err != nil || x.message.Content != "done" {
		t.Fatal("error retry changed after restart", failure)
	}
	// The same bytes reported as success represent a changed request.
	req.Messages[len(req.Messages)-1].IsError = false
	code, next := postGateway(t, server, req, "")
	if code != 200 || len(returnedMessage(t, next).ToolCalls) != 1 || starts.Load() != 2 {
		t.Fatal("changed status replayed the old answer", code, starts.Load())
	}
}

func TestMixedBatchFailureAndHistoryKeys(t *testing.T) {
	p := &clientToolPending{batchCalls: []apiToolCall{{ID: "a", Function: apiCallFunction{Name: "f"}}, {ID: "b", Function: apiCallFunction{Name: "g"}}}}
	results := []apiMessage{{Role: "tool", ToolCallID: "b", Content: "same"}, {Role: "tool", ToolCallID: "a", Content: "same", IsError: true}}
	value, failure := continuationValue(p, results, "")
	if failure != nil {
		t.Fatal(failure)
	}
	rows := array(field(parsed(value), "results"))
	if field(rows[0], "isError") != true || field(rows[1], "isError") != nil {
		t.Fatal("batch error status not associated with exact call", value)
	}
	c, _ := newClient(t.TempDir())
	native, err := c.nativeToolReply(context.Background(), object{"id": "n"}, value, true, false)
	if err != nil || field(parsed(native["raw_result"]), "isError") != false || !strings.Contains(textValue(field(array(field(parsed(native["raw_result"]), "content"))[0], "text")), `"isError":true`) {
		t.Fatal("mixed batch failure lost", err, native)
	}
	a := results[1]
	a.IsError = false
	if threadMessage(a) == threadMessage(results[1]) {
		t.Fatal("failure and success share history key")
	}
}

func TestLargeHistoryUsesSmallDeltaAndRejectsUnsafeFallback(t *testing.T) {
	g, server := testGateway(t)
	chat := &fakeChat{}
	g.c.http.Transport = chat
	firstText, secondText := strings.Repeat("甲", 180000), strings.Repeat("乙", 100000)
	code, response := postGateway(t, server, chatRequest(firstText), "")
	if code != 200 {
		t.Fatal(code, response)
	}
	first := textValue(returnedMessage(t, response).Content)
	code, response = postGateway(t, server, chatRequest(firstText, first, secondText), "")
	if code != 200 || sentText(chat.last()) != secondText {
		t.Fatal("valid delta refused or full history resent", code)
	}
	second := textValue(returnedMessage(t, response).Content)
	base := chatRequest(firstText, first, secondText, second, "valid delta")
	invalid := base
	invalid.Messages = append([]apiMessage{}, base.Messages...)
	invalid.Messages[4].Content = strings.Repeat("😀", maxPromptChars/2+1)
	before := len(chat.bodies)
	code, response = postGateway(t, server, invalid, "")
	if code != 400 || field(response, "error", "code") != "context_length_exceeded" || len(chat.bodies) != before {
		t.Fatal("oversized delta reached upstream", code)
	}
	// The rejected local request must not consume the native thread. If the
	// server now rejects that thread, oversized full history is never posted.
	chat.reject = func(body object) int {
		if textValue(field(body, "client_meta", "conversation_id")) != "" {
			return 710000001
		}
		return 0
	}
	code, response = postGateway(t, server, base, "")
	if code != 400 || field(response, "error", "code") != "context_length_exceeded" || len(chat.bodies) != before+1 || sentText(chat.last()) != "valid delta" {
		t.Fatal("unsafe fallback resubmitted history or lost typed error", code, response, len(chat.bodies)-before)
	}
}
