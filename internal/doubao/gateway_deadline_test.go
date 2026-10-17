package doubao

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// chainRunner forwards calls caller tools in turn, generating for work before
// each one and once more before answering with the last result. Like the real
// runner, a task stopped meanwhile finishes with its abort.
func chainRunner(g *gateway, starts *atomic.Int32, calls int, work func(step int) time.Duration) func(*gatewayTurn) {
	return func(turn *gatewayTurn) {
		starts.Add(1)
		g.mu.Lock()
		turn.connector = "fixture-connector"
		turn.generationPosts++
		g.mu.Unlock()
		stop := func(err error) {
			g.mu.Lock()
			abort := turn.abort
			g.mu.Unlock()
			if abort == nil {
				abort = apiFail(502, "fixture", err.Error())
			}
			g.finish(turn, "", abort)
		}
		generate := func(step int) bool {
			select {
			case <-time.After(work(step)):
				return true
			case <-turn.ctx.Done():
				stop(turn.ctx.Err())
				return false
			}
		}
		if err := saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "1234567890123", RunID: "1234567890124"}); err != nil {
			stop(err)
			return
		}
		text := ""
		for i := 0; i < calls; i++ {
			if !generate(i) {
				return
			}
			result, err := g.forwardClientTool(turn.ctx, turn, object{"id": fmt.Sprint("native-call-", i), "name": "connector.call", "input": "native-input"}, object{"protocol_version": 1, "connector_id": "fixture-connector", "remote_tool_name": "client_nonce", "raw_arguments": `{"challenge":"unit"}`})
			if err != nil {
				stop(err)
				return
			}
			text = textValue(field(array(field(parsed(result["raw_result"]), "content"))[0], "text"))
		}
		if generate(calls) {
			g.finish(turn, text, nil)
		}
	}
}

// answerTool appends the assistant's tool call and a result for it.
func answerTool(t *testing.T, req apiRequest, response object, value string) apiRequest {
	t.Helper()
	assistant := returnedMessage(t, response)
	if len(assistant.ToolCalls) != 1 {
		t.Fatal("expected one tool call", response)
	}
	req.Messages = append(append([]apiMessage{}, req.Messages...), assistant, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: value})
	return req
}

func instant(int) time.Duration { return 0 }

func TestGatewayTimeoutAppliesToEachWaitNotTheWholeChain(t *testing.T) {
	g, server := testGateway(t)
	g.taskTimeout = 600 * time.Millisecond
	var starts atomic.Int32
	g.run = chainRunner(g, &starts, 3, instant)
	req := toolRequest()
	code, response := postGateway(t, server, req, "")
	// Each result arrives within the timeout, the chain only after it.
	for i := 0; i < 3; i++ {
		if code != 200 {
			t.Fatal(i, code, response)
		}
		time.Sleep(400 * time.Millisecond)
		req = answerTool(t, req, response, fmt.Sprint("value-", i))
		code, response = postGateway(t, server, req, "")
	}
	if code != 200 || textValue(returnedMessage(t, response).Content) != "value-2" || starts.Load() != 1 {
		t.Fatal("the chain did not finish in one task", code, response, starts.Load())
	}
}

func TestGatewayWorkThatOutlivesTheTimeoutExpires(t *testing.T) {
	g, server := testGateway(t)
	g.taskTimeout = 300 * time.Millisecond
	var starts atomic.Int32
	// Work answers the first call at once, then takes too long on its result.
	g.run = chainRunner(g, &starts, 1, func(step int) time.Duration { return time.Duration(step) * 5 * time.Second })
	req := toolRequest()
	_, response := postGateway(t, server, req, "")
	time.Sleep(200 * time.Millisecond)
	code, response := postGateway(t, server, answerTool(t, req, response, "value"), "")
	if code != 504 || field(response, "error", "code") != "task_expired" || starts.Load() != 1 {
		t.Fatal("slow continuation did not expire", code, response, starts.Load())
	}
}

func TestGatewayResultAfterTheTimeoutRestartsTheTask(t *testing.T) {
	g, server := testGateway(t)
	g.taskTimeout = 300 * time.Millisecond
	var starts atomic.Int32
	g.run = chainRunner(g, &starts, 1, instant)
	req := toolRequest()
	_, response := postGateway(t, server, req, "")
	time.Sleep(600 * time.Millisecond)
	expired := 0
	g.mu.Lock()
	for _, turn := range g.turns {
		if turn.done && turn.err != nil && turn.err.Code == "task_expired" {
			expired++
		}
	}
	g.mu.Unlock()
	if expired != 1 {
		t.Fatal("parked task did not expire", expired)
	}
	// The caller never sees the expiry: its result starts a new task over the
	// full transcript.
	code, response := postGateway(t, server, answerTool(t, req, response, "late value"), "")
	if code != 200 || len(returnedMessage(t, response).ToolCalls) != 1 || starts.Load() != 2 {
		t.Fatal("late result did not restart the task", code, response, starts.Load())
	}
}

func TestGatewayRenewalKeepsAnExpiredTaskExpired(t *testing.T) {
	g, _ := testGateway(t)
	g.taskTimeout = time.Minute
	ctx, cancel := context.WithCancel(g.ctx)
	turn := &gatewayTurn{id: "renewal", ctx: ctx, cancel: cancel, deadline: time.Now().Add(time.Second)}
	g.mu.Lock()
	g.armLocked(turn)
	renewed := g.renewLocked(turn)
	deadline := turn.deadline
	g.mu.Unlock()
	if !renewed || time.Until(deadline) < 50*time.Second {
		t.Fatal("running task was not renewed", renewed, deadline)
	}
	g.mu.Lock()
	turn.deadline = time.Now()
	turn.timer.Reset(0)
	g.mu.Unlock()
	<-ctx.Done()
	g.mu.Lock()
	defer g.mu.Unlock()
	if turn.abort == nil || turn.abort.Code != "task_expired" || turn.abort.Status != 504 || g.renewLocked(turn) {
		t.Fatal("expired task was renewed", turn.abort)
	}
}

func TestGatewayLongToolChainsMoveToANewTask(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	g.run = chainRunner(g, &starts, 100, instant)
	req := toolRequest()
	code, response := postGateway(t, server, req, "")
	for i := 1; i <= maxForwarded; i++ {
		if code != 200 || starts.Load() != 1 {
			t.Fatal(i, code, response, starts.Load())
		}
		req = answerTool(t, req, response, fmt.Sprint("value-", i))
		code, response = postGateway(t, server, req, "")
	}
	// The 32nd result reaches a task that may forward no more calls; the
	// caller gets the next call from a new task instead of an error.
	superseded := 0
	g.mu.Lock()
	for _, turn := range g.turns {
		if turn.abort != nil && turn.abort.Code == "task_superseded" && turn.forwarded == maxForwarded {
			superseded++
		}
	}
	g.mu.Unlock()
	if code != 200 || len(returnedMessage(t, response).ToolCalls) != 1 || starts.Load() != 2 || superseded != 1 {
		t.Fatal("exhausted task was not replaced", code, response, starts.Load(), superseded)
	}
}

func TestGatewayBatchTasksMoveOnBeforeABatchCouldExceedTheLimit(t *testing.T) {
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	for forwarded, want := range map[int]bool{24: false, 25: true} {
		if exhaustedLocked(&gatewayTurn{request: req, forwarded: forwarded}) != want {
			t.Fatal("batch limit", forwarded)
		}
	}
	if exhaustedLocked(&gatewayTurn{request: toolRequest(), forwarded: maxForwarded - 1}) {
		t.Fatal("single-call task moved on early")
	}
}
