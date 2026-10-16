package doubao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGateway(t *testing.T) (*gateway, *httptest.Server) {
	t.Helper()
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := newGateway(ctx, c, []Model{{Name: "TestModel", Available: true, Modes: []string{"work"}}}, c.dir, time.Minute)
	g.apiKey = ""
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); server.Close(); g.wg.Wait() })
	return g, server
}
func toolRequest() apiRequest {
	return apiRequest{Model: "TestModel", Messages: []apiMessage{{Role: "user", Content: "Use the client tool"}}, Tools: []apiTool{{Type: "function", Function: apiFunction{Name: "client_nonce", Parameters: object{"type": "object", "properties": object{"challenge": object{"type": "string"}}, "required": []string{"challenge"}}}}}}
}
func postGateway(t *testing.T, server *httptest.Server, req apiRequest, key string) (int, object) {
	t.Helper()
	r, err := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(jsonText(req)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Transport: server.Client().Transport, Timeout: 5 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value object
	if err = decode(response.Body, &value); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, value
}
func returnedMessage(t *testing.T, response object) apiMessage {
	t.Helper()
	choices := array(response["choices"])
	if len(choices) != 1 {
		t.Fatal(response)
	}
	var message apiMessage
	if err := decode(strings.NewReader(jsonText(field(choices[0], "message"))), &message); err != nil {
		t.Fatal(err)
	}
	return message
}
func fakeToolRunner(g *gateway, starts, results *atomic.Int32) func(*gatewayTurn) {
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
		result, err := g.forwardClientTool(turn.ctx, turn, object{"id": "native-call", "name": "connector.call", "input": "native-input"}, object{"protocol_version": 1, "connector_id": "fixture-connector", "remote_tool_name": "client_nonce", "raw_arguments": `{"challenge":"unit"}`})
		if err != nil {
			g.finish(turn, "", apiFail(502, "fixture", err.Error()))
			return
		}
		results.Add(1)
		content := array(field(parsed(result["raw_result"]), "content"))
		g.finish(turn, textValue(field(content[0], "text")), nil)
	}
}

func TestGatewayExternalToolsResumeOriginalTaskAndDeduplicate(t *testing.T) {
	g, server := testGateway(t)
	var starts, results atomic.Int32
	g.run = fakeToolRunner(g, &starts, &results)
	req := toolRequest()
	code, first := postGateway(t, server, req, "initial-retry")
	if code != 200 || field(array(first["choices"])[0], "finish_reason") != "tool_calls" {
		t.Fatal(code, first)
	}
	assistant := returnedMessage(t, first)
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "client_nonce" {
		t.Fatal(first)
	}
	_, replay := postGateway(t, server, req, "initial-retry")
	if jsonText(first) != jsonText(replay) {
		t.Fatal("retry did not reuse the original tool call")
	}
	conflict := req
	conflict.Messages = []apiMessage{{Role: "user", Content: "Different input"}}
	if code, _ = postGateway(t, server, conflict, "initial-retry"); code != 409 {
		t.Fatal("conflicting idempotency key accepted")
	}
	continuation := req
	continuation.Messages = append(append([]apiMessage{}, req.Messages...), assistant, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: "client-generated-value"})
	bad := continuation
	bad.Messages = append([]apiMessage{}, continuation.Messages...)
	bad.Messages[0].Content = "Altered history"
	if code, _ = postGateway(t, server, bad, ""); code != 409 {
		t.Fatal("altered history accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, answer := postGateway(t, server, continuation, "")
			if status != 200 || returnedMessage(t, answer).Content != "client-generated-value" {
				t.Error(status, answer)
			}
		}()
	}
	wg.Wait()
	if starts.Load() != 1 || results.Load() != 1 {
		t.Fatal("task or result was duplicated", starts.Load(), results.Load())
	}
	bad = continuation
	bad.Messages = append([]apiMessage{}, continuation.Messages...)
	bad.Messages[len(bad.Messages)-1].Content = "Conflicting result"
	if code, _ = postGateway(t, server, bad, ""); code != 409 {
		t.Fatal("conflicting tool result accepted")
	}
}

func TestGatewayToolStatePersistenceFailureCanBeRetriedWithoutUpload(t *testing.T) {
	g, server := testGateway(t)
	var starts, results atomic.Int32
	g.run = fakeToolRunner(g, &starts, &results)
	req := toolRequest()
	_, first := postGateway(t, server, req, "")
	assistant := returnedMessage(t, first)
	call := assistant.ToolCalls[0]
	continuation := req
	continuation.Messages = append(append([]apiMessage{}, req.Messages...), assistant, apiMessage{Role: "tool", ToolCallID: call.ID, Content: "persisted-result"})
	g.mu.Lock()
	pending := g.calls[call.ID]
	path := pending.turn.statePath
	pending.turn.statePath = filepath.Join(g.dir, "missing", "state.json")
	g.mu.Unlock()
	if code, _ := postGateway(t, server, continuation, ""); code != 500 {
		t.Fatal("persistence failure was ignored")
	}
	if results.Load() != 0 {
		t.Fatal("uploaded an unpersisted tool result")
	}
	g.mu.Lock()
	pending.turn.statePath = path
	g.mu.Unlock()
	code, answer := postGateway(t, server, continuation, "")
	if code != 200 || returnedMessage(t, answer).Content != "persisted-result" || starts.Load() != 1 || results.Load() != 1 {
		t.Fatal("safe retry failed", code, answer)
	}
}

func TestGatewayClientDisconnectKeepsIdempotentTask(t *testing.T) {
	g, server := testGateway(t)
	started, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	g.run = func(turn *gatewayTurn) {
		starts.Add(1)
		close(started)
		select {
		case <-release:
			g.finish(turn, "same-task", nil)
		case <-turn.ctx.Done():
			g.finish(turn, "", apiFail(504, "expired", "expired"))
		}
	}
	req := apiRequest{Model: "TestModel", Messages: []apiMessage{{Role: "user", Content: "wait"}}}
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(jsonText(req)))
	r.Header.Set("Idempotency-Key", "disconnect-test")
	done := make(chan struct{})
	go func() {
		response, _ := server.Client().Do(r)
		if response != nil {
			response.Body.Close()
		}
		close(done)
	}()
	<-started
	cancel()
	<-done
	close(release)
	code, answer := postGateway(t, server, req, "disconnect-test")
	if code != 200 || returnedMessage(t, answer).Content != "same-task" || starts.Load() != 1 {
		t.Fatal("client disconnect created a second task", code, answer)
	}
}

func readGatewayStream(t *testing.T, server *httptest.Server, req apiRequest) []object {
	t.Helper()
	response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(jsonText(req)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		data, _ := io.ReadAll(response.Body)
		t.Fatal(response.StatusCode, string(data))
	}
	s := bufio.NewScanner(response.Body)
	chunks := []object{}
	done := false
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if line == "data: [DONE]" {
			done = true
			continue
		}
		var chunk object
		if err = decode(strings.NewReader(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if s.Err() != nil || !done {
		t.Fatal("SSE response did not terminate", s.Err())
	}
	return chunks
}

func TestGatewaySSEToolsAndFinalAnswerUseOpenAIChunks(t *testing.T) {
	g, server := testGateway(t)
	var starts, results atomic.Int32
	g.run = fakeToolRunner(g, &starts, &results)
	req := toolRequest()
	req.Stream = true
	chunks := readGatewayStream(t, server, req)
	if len(chunks) != 3 || field(array(chunks[0]["choices"])[0], "delta", "role") != "assistant" || field(array(chunks[2]["choices"])[0], "finish_reason") != "tool_calls" {
		t.Fatal(chunks)
	}
	callRaw := array(field(array(chunks[1]["choices"])[0], "delta", "tool_calls"))[0]
	var call apiToolCall
	if err := decode(strings.NewReader(jsonText(callRaw)), &call); err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, apiMessage{Role: "assistant", ToolCalls: []apiToolCall{call}}, apiMessage{Role: "tool", ToolCallID: call.ID, Content: "stream-result"})
	chunks = readGatewayStream(t, server, req)
	if len(chunks) != 3 || field(array(chunks[1]["choices"])[0], "delta", "content") != "stream-result" || field(array(chunks[2]["choices"])[0], "finish_reason") != "stop" || results.Load() != 1 {
		t.Fatal(chunks)
	}
}

func TestGatewayRejectsUnknownCallsUnsupportedSemanticsAndUnauthorizedRequests(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	g.run = func(turn *gatewayTurn) { starts.Add(1); g.finish(turn, "unexpected", nil) }
	req := toolRequest()
	req.Messages = []apiMessage{{Role: "tool", ToolCallID: "call_unknown", Content: "result"}}
	if code, _ := postGateway(t, server, req, ""); code != 409 {
		t.Fatal("unknown call accepted")
	}
	for _, field := range []object{{"tool_choice": "required"}, {"temperature": 0.2}, {"n": 2}, {"stream_options": object{"include_usage": true}}} {
		var value object
		json.Unmarshal([]byte(jsonText(toolRequest())), &value)
		for k, v := range field {
			value[k] = v
		}
		response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(jsonText(value)))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("unsupported semantics were silently accepted", field, response.StatusCode)
		}
	}
	if starts.Load() != 0 {
		t.Fatal("invalid requests submitted upstream work")
	}
	g.apiKey = "test-key"
	response, err := server.Client().Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("missing API key accepted")
	}
}

func TestGatewayMCPDescriptorNeverExecutesClientFunctions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := saveJSON(path, []any{nonceTool()}); err != nil {
		t.Fatal(err)
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_nonce","arguments":{"challenge":"never-execute"}}}` + "\n"
	var output bytes.Buffer
	if err := serveToolDescriptor(strings.NewReader(input), &output, path); err != nil {
		t.Fatal(err)
	}
	var response object
	if err := decode(&output, &response); err != nil {
		t.Fatal(err)
	}
	if field(response, "result", "isError") != true || strings.Contains(jsonText(response), "DOUBAO_TOOL_") {
		t.Fatal("descriptor executed the caller's tool", response)
	}
}

func TestGatewayConnectorCleanupWaitsForReadbackWithoutRepeatingMutation(t *testing.T) {
	g, _ := testGateway(t)
	mutations, reads := 0, 0
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":0}`
		if strings.HasSuffix(req.URL.Path, "/disconnect") {
			mutations++
		} else if strings.HasSuffix(req.URL.Path, "/list_by_user") {
			reads++
			body = `{"code":0,"data":{"items":[],"has_more":false}}`
			if reads == 1 {
				body = `{"code":0,"data":{"items":[{"connector_id":"fixture-connector"}],"has_more":false}}`
			}
		} else {
			t.Fatal("unexpected cleanup request", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := &gatewayTurn{id: "cleanup", ctx: ctx, cancel: cancel, connector: "fixture-connector", statePath: filepath.Join(g.dir, "cleanup.json")}
	if err := g.removeConnector(turn); err != nil {
		t.Fatal(err)
	}
	if mutations != 1 || reads != 2 || !turn.cleanupConfirmed {
		t.Fatal("cleanup was repeated or not confirmed", mutations, reads)
	}
	if err := g.removeConnector(turn); err != nil {
		t.Fatal(err)
	}
	if mutations != 1 {
		t.Fatal("confirmed cleanup was mutated a second time")
	}
}
