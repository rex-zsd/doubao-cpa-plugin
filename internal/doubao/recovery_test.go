package doubao

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type untilCancelled struct{ ctx context.Context }

func (r untilCancelled) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r untilCancelled) Close() error             { return nil }

func persistedTurn(t *testing.T, g *gateway, done bool) *gatewayTurn {
	t.Helper()
	id := token()
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(g.ctx, deadline)
	x := newExchange("TestModel")
	req := toolRequest()
	req.Parallel = new(bool)
	*req.Parallel = true
	turn := &gatewayTurn{id: id, deadline: deadline, ctx: ctx, cancel: cancel, request: req, first: x, current: x, generationPosts: 1, statePath: filepath.Join(g.dir, "gateway-state-"+id+".json"), receiptPath: filepath.Join(g.dir, "gateway-receipt-"+id+".json"), initialKey: apiHash("restart"), initialHash: apiHash(object{"model": req.Model, "messages": req.Messages, "tools": req.Tools, "tool_choice": req.ToolChoice, "parallel": parallelRequested(req)})}
	g.turns[id] = turn
	if done {
		turn.done = true
		turn.finished = time.Now()
		turn.finalText = "cached answer"
		x.resolve(apiMessage{Role: "assistant", Content: turn.finalText}, "stop", nil)
	}
	if err := g.checkpointLocked(turn); err != nil {
		t.Fatal(err)
	}
	return turn
}
func TestRecoveryCompletedReplayPreservesIdentityWithoutNetwork(t *testing.T) {
	c, _ := newClient(t.TempDir())
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, true)
	calls := atomic.Int32{}
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, context.Canceled })
	restored := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	defer restored.turns[turn.id].cancel()
	x, failure := restored.submit(turn.request, "restart")
	if failure != nil {
		t.Fatal(failure)
	}
	if x.id != turn.first.id || x.message.Content != "cached answer" || calls.Load() != 0 {
		t.Fatal("completed replay changed or performed network")
	}
}
func TestRecoveryUncertainResultNeverUploadsOrGenerates(t *testing.T) {
	c, _ := newClient(t.TempDir())
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, false)
	_ = saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "conv", RunID: "run"})
	_ = saveJSON(filepath.Join(c.dir, "gateway-native-"+turn.id+".json"), object{"1:native": object{"uploaded": false, "upload_body": object{"result": "unknown"}}})
	var calls atomic.Int32
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.Canceled })
	restored := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	restored.wg.Wait()
	x, failure := restored.submit(turn.request, "restart")
	if failure != nil {
		t.Fatal(failure)
	}
	if x.err == nil || x.err.Code != "native_write_unconfirmed" || calls.Load() != 0 {
		t.Fatal("uncertain native write was retried")
	}
	turn.cancel()
}
func TestRecoveryResumesPendingBatchOnceAndCachesTerminalReplay(t *testing.T) {
	c, _ := newClient(t.TempDir())
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, false)
	turn.connector, turn.sandbox = "connector", "sandbox"
	native := object{"id": "native", "name": "connector.call", "input": jsonText(object{"protocol_version": 1, "connector_id": "connector", "remote_tool_name": batchToolName, "raw_arguments": jsonText(object{"calls": []any{object{"name": "client_nonce", "arguments": object{}}}})})}
	calls := []apiToolCall{{ID: "a", Type: "function", Function: apiCallFunction{Name: "client_nonce", Arguments: "{}"}}, {ID: "b", Type: "function", Function: apiCallFunction{Name: "client_nonce", Arguments: "{}"}}}
	assistant := apiMessage{Role: "assistant", Content: nil, ToolCalls: calls}
	pending := &clientToolPending{turn: turn, nativeID: "native", exposed: turn.first, batchCalls: calls, call: calls[0], expected: append(append([]apiMessage{}, turn.request.Messages...), assistant), result: make(chan string, 1)}
	for _, call := range calls {
		g.calls[call.ID] = pending
	}
	turn.forwarded = 2
	turn.first.resolve(assistant, "tool_calls", nil)
	frame := object{"event_type": 1, "sandbox_id": "sandbox", "tool_call": native}
	_ = saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "conv", RunID: "run"})
	_ = saveJSON(filepath.Join(c.dir, "gateway-native-"+turn.id+".json"), object{"1:native": object{"uploaded": false, "frame": frame}})
	if err := g.checkpointLocked(turn); err != nil {
		t.Fatal(err)
	}
	var uploads, generations atomic.Int32
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		status := object{"code": 0}
		header := http.Header{}
		var body io.ReadCloser
		switch r.URL.Path {
		case "/chat/completion":
			generations.Add(1)
			return nil, context.Canceled
		case "/alice/office/tool_local/chunk_stream":
			header.Set("Content-Type", "text/event-stream")
			body = io.NopCloser(io.MultiReader(strings.NewReader("data: "+jsonText(frame)+"\n\n"), untilCancelled{r.Context()}))
		case "/alice/office/tool_local/upload_tool_call_result":
			uploads.Add(1)
		case "/alice/office/skills/manage/connector/list_by_user":
			status = object{"code": 0, "data": object{"items": []any{}, "has_more": false}}
		case "/alice/office/skills/manage/connector/disconnect":
		case "/im/conversation/batch_get":
			messages := []any{object{"user_type": 1, "message_id": "run"}}
			if uploads.Load() > 0 {
				messages = append(messages, object{"user_type": 2, "message_id": "reply", "bot_reply_message_id": "run", "status": 0, "content_block": []any{object{"content": object{"text_block": object{"text": "recovered result"}}}}, "ext": object{"is_finish": "1"}})
			}
			status = object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": "conv", "messages": messages}}}}}
		default:
			t.Errorf("unexpected recovery route %s", r.URL.Path)
		}
		if body == nil {
			body = io.NopCloser(strings.NewReader(jsonText(status)))
		}
		return &http.Response{StatusCode: 200, Header: header, Body: body, Request: r}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := newGateway(ctx, c, nil, c.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	replay, failure := restored.submit(turn.request, "restart")
	if failure != nil || jsonText(replay.message) != jsonText(assistant) {
		t.Fatal("call identities changed", failure)
	}
	continuation := turn.request
	continuation.Messages = append(append([]apiMessage{}, pending.expected...), apiMessage{Role: "tool", ToolCallID: "b", Content: "value b"}, apiMessage{Role: "tool", ToolCallID: "a", Content: "value a"})
	x, failure := restored.submit(continuation, "")
	if failure != nil {
		t.Fatal(failure)
	}
	select {
	case <-x.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered task stalled")
	}
	restored.wg.Wait()
	if x.err != nil || x.message.Content != "recovered result" || generations.Load() != 0 || uploads.Load() != 1 {
		t.Fatal("recovery duplicated writes or failed", x.err, generations.Load(), uploads.Load())
	}
	again, failure := restored.submit(continuation, "")
	if failure != nil || again != x {
		t.Fatal("tool replay was not cached")
	}
	terminal := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	if err := terminal.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	terminal.wg.Wait()
	final, failure := terminal.submit(continuation, "")
	if failure != nil || final.message.Content != "recovered result" || uploads.Load() != 1 {
		t.Fatal("terminal restart lost continuation", failure)
	}
	turn.cancel()
	// Ensure cleanup was confirmed and the original pending journal wasn't lost.
	if _, err := os.Stat(turn.statePath); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryPreservesExpiredDeadlineAndStopsOnlyOnce(t *testing.T) {
	c, _ := newClient(t.TempDir())
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, false)
	turn.deadline = time.Now().Add(-time.Minute)
	_ = g.checkpointLocked(turn)
	_ = saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "conv", RunID: "run"})
	var writes, generations atomic.Int32
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		value := object{"status_code": 0}
		switch r.URL.Path {
		case "/passport/account/info/v2/":
			value = object{"data": object{"user_id_str": "account"}}
		case "/im/conversation/batch_get":
			value = object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": "conv", "messages": []any{object{"message_id": "run", "user_type": 1}}}}}}}
		case "/im/message/break_stream_msg":
			writes.Add(1)
		case "/chat/completion":
			generations.Add(1)
		default:
			t.Errorf("unexpected expiry route %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jsonText(value))), Header: http.Header{}}, nil
	})
	for i := 0; i < 2; i++ {
		restored := newGateway(context.Background(), c, nil, c.dir, time.Minute)
		if err := restored.recoverTasks(); err != nil {
			t.Fatal(err)
		}
		restored.wg.Wait()
		saved := restored.turns[turn.id]
		if saved.err == nil || saved.err.Code != "task_expired" || saved.deadline.Unix() != turn.deadline.Unix() {
			t.Fatal("deadline reset on recovery")
		}
	}
	if writes.Load() != 1 || generations.Load() != 0 {
		t.Fatal("expiry duplicated generation or cancellation", writes.Load(), generations.Load())
	}
	turn.cancel()
}
