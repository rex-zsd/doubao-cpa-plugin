package doubao

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// reissueFixture runs a task over a real native tool channel. feed sends Work
// frames for one identical client call; the runner finishes with "done" after
// the given number of result uploads, which it returns in order.
type reissueFixture struct {
	g       *gateway
	frames  chan string
	uploads chan object
	results chan []object
}

func newReissueFixture(t *testing.T, g *gateway, want int, afterFirst func(*gatewayTurn)) *reissueFixture {
	t.Helper()
	newest, writer := io.Pipe()
	f := &reissueFixture{g: g, frames: make(chan string, 8), uploads: make(chan object, 8), results: make(chan []object, 1)}
	go func() {
		for frame := range f.frames {
			if _, err := io.WriteString(writer, frame); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { close(f.frames) })
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/alice/office/tool_local/chunk_stream" {
			go func() { <-req.Context().Done(); writer.CloseWithError(req.Context().Err()) }()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: newest}, nil
		}
		var body object
		if err := decode(req.Body, &body); err != nil {
			t.Error(err)
		}
		f.uploads <- body
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	g.run = func(turn *gatewayTurn) {
		g.mu.Lock()
		turn.connector, turn.sandbox = "fixture-connector", "fixture-sandbox"
		turn.generationPosts++
		g.mu.Unlock()
		if err := saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: "1234567890123", RunID: "1234567890124"}); err != nil {
			g.finish(turn, "", apiFail(500, "fixture", err.Error()))
			return
		}
		ctx, cancel := context.WithCancel(turn.ctx)
		done := g.serveTools(ctx, ctx, cancel, g.taskProbe(turn, "fixture-sandbox", g.dir, "fixture-connector"), nil)
		f.feed("native-first")
		got := []object{}
		for len(got) < want {
			select {
			case body := <-f.uploads:
				got = append(got, body)
				if len(got) == 1 && afterFirst != nil {
					afterFirst(turn)
				}
			case err := <-done:
				g.finish(turn, "", apiFail(502, "fixture", "tool channel stopped: "+errText(err)))
				f.results <- got
				return
			}
		}
		cancel()
		<-done
		f.results <- got
		g.finish(turn, "done", nil)
	}
	return f
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

func (f *reissueFixture) feed(id string) {
	input := jsonText(object{"protocol_version": 1, "connector_id": "fixture-connector", "remote_tool_name": "client_nonce", "raw_arguments": `{"challenge":"unit"}`})
	frame := object{"event_type": 1, "sandbox_id": "fixture-sandbox", "tool_call": object{"id": id, "name": "connector.call", "input": input}}
	f.frames <- "data: " + jsonText(frame) + "\n\n"
}

// await polls gateway state, which the channel reader updates on arrival.
func await(t *testing.T, g *gateway, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		g.mu.Lock()
		done := ok()
		g.mu.Unlock()
		if done {
			return
		}
	}
	t.Fatal("timed out waiting until", what)
}

func uploadedResult(t *testing.T, body object) (string, string, bool) {
	t.Helper()
	result := parsed(field(body, "tool_result", "raw_result"))
	return textValue(field(body, "tool_result", "id")), textValue(field(array(field(result, "content"))[0], "text")), field(result, "isError") == true
}

// Work waits about a minute for a client function, then sends the same call
// under a new native ID. That re-issue arrives while the original is still
// unanswered: it gets the original's result and error flag, and the caller
// never sees a second tool call.
func TestWorkReissueWhileOriginalRunsReusesItsResult(t *testing.T) {
	g, server := testGateway(t)
	f := newReissueFixture(t, g, 2, nil)
	req := toolRequest()
	code, exposed := postGateway(t, server, req, "")
	if code != 200 || field(array(exposed["choices"])[0], "finish_reason") != "tool_calls" {
		t.Fatal(code, exposed)
	}
	f.feed("native-reissue")
	await(t, g, "the re-issue is recorded as a twin", func() bool {
		for _, turn := range g.turns {
			if turn.twins["native-reissue"] == "native-first" {
				return true
			}
		}
		return false
	})
	assistant := returnedMessage(t, exposed)
	continuation := req
	continuation.Messages = append(append([]apiMessage{}, req.Messages...), assistant, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: "boom", IsError: true})
	code, answer := postGateway(t, server, continuation, "")
	if code != 200 || field(array(answer["choices"])[0], "finish_reason") != "stop" || returnedMessage(t, answer).Content != "done" {
		t.Fatal("the re-issue reached the caller instead of reusing the result", code, answer)
	}
	g.wg.Wait()
	got := <-f.results
	for i, want := range []string{"native-first", "native-reissue"} {
		id, text, isError := uploadedResult(t, got[i])
		if id != want || text != "boom" || !isError {
			t.Fatal("upload", i, "did not carry the original result for", want, id, text, isError)
		}
	}
	for _, turn := range g.turns {
		if turn.reissues != 1 || len(turn.open) != 0 || len(turn.twins) != 0 {
			t.Fatal("re-issue bookkeeping", turn.reissues, turn.open, turn.twins)
		}
	}
}

// The same call sent after the original was answered is a new call: Work may
// legitimately repeat a function, so it reaches the caller and runs again.
func TestIdenticalCallAfterAnswerRunsAgain(t *testing.T) {
	g, server := testGateway(t)
	var f *reissueFixture
	f = newReissueFixture(t, g, 2, func(turn *gatewayTurn) {
		await(t, g, "the original call is closed", func() bool { return len(turn.open) == 0 })
		f.feed("native-again")
	})
	req := toolRequest()
	code, first := postGateway(t, server, req, "")
	if code != 200 || field(array(first["choices"])[0], "finish_reason") != "tool_calls" {
		t.Fatal(code, first)
	}
	callOne := returnedMessage(t, first)
	mid := req
	mid.Messages = append(append([]apiMessage{}, req.Messages...), callOne, apiMessage{Role: "tool", ToolCallID: callOne.ToolCalls[0].ID, Content: "value-one"})
	code, second := postGateway(t, server, mid, "")
	if code != 200 || field(array(second["choices"])[0], "finish_reason") != "tool_calls" {
		t.Fatal("a repeated call after the answer was not exposed", code, second)
	}
	callTwo := returnedMessage(t, second)
	if callTwo.ToolCalls[0].ID == callOne.ToolCalls[0].ID {
		t.Fatal("second call reused the first exposure", second)
	}
	last := mid
	last.Messages = append(append([]apiMessage{}, mid.Messages...), callTwo, apiMessage{Role: "tool", ToolCallID: callTwo.ToolCalls[0].ID, Content: "value-two"})
	code, answer := postGateway(t, server, last, "")
	if code != 200 || returnedMessage(t, answer).Content != "done" {
		t.Fatal("task did not finish after two calls", code, answer)
	}
	g.wg.Wait()
	got := <-f.results
	for i, want := range [][2]string{{"native-first", "value-one"}, {"native-again", "value-two"}} {
		id, text, isError := uploadedResult(t, got[i])
		if id != want[0] || text != want[1] || isError {
			t.Fatal("upload", i, "carried the wrong result", id, text, isError)
		}
	}
	for _, turn := range g.turns {
		if turn.reissues != 0 {
			t.Fatal("a new call was counted as a re-issue", turn.reissues)
		}
	}
}

func TestCallSignatureComparesArgumentValues(t *testing.T) {
	call := func(name, args string) object {
		return object{"id": "native-" + args, "name": name, "input": jsonText(object{"protocol_version": 1, "connector_id": "c", "remote_tool_name": "client_nonce", "raw_arguments": args})}
	}
	same := callSignature(call("connector.call", `{"a":1,"b":"x"}`))
	if callSignature(call("connector.call", `{ "b": "x", "a": 1 }`)) != same {
		t.Fatal("argument encoding changed the signature")
	}
	for _, other := range []object{call("connector.call", `{"a":2,"b":"x"}`), call("Bash", `{"a":1,"b":"x"}`)} {
		if callSignature(other) == same {
			t.Fatal("a different call shares the signature", other)
		}
	}
	native := object{"id": "n1", "name": "Bash", "input": `{"command":"ls"}`}
	if callSignature(native) != callSignature(object{"id": "n2", "name": "Bash", "input": `{"command":"ls"}`}) {
		t.Fatal("native call signature depends on its ID")
	}
}
