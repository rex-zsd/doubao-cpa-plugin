package doubao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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
	// A conversation the task can no longer continue replaces it with a new
	// task over the full transcript; identical retries share that task.
	bad := continuation
	bad.Messages = append([]apiMessage{}, continuation.Messages...)
	bad.Messages[len(bad.Messages)-1].Content = "Conflicting result"
	code, replaced := postGateway(t, server, bad, "")
	if code != 200 || field(array(replaced["choices"])[0], "finish_reason") != "tool_calls" || starts.Load() != 2 {
		t.Fatal("conflicting tool result did not start a new task", code, replaced, starts.Load())
	}
	if _, retry := postGateway(t, server, bad, ""); jsonText(retry) != jsonText(replaced) || starts.Load() != 2 {
		t.Fatal("transcript retry started another task", retry, starts.Load())
	}
	bad = continuation
	bad.Messages = append([]apiMessage{}, continuation.Messages...)
	bad.Messages[0].Content = "Altered history"
	if code, _ = postGateway(t, server, bad, ""); code != 200 || starts.Load() != 3 {
		t.Fatal("altered history did not start a new task", code, starts.Load())
	}
}

func TestGatewayToolResultsWithTrailingUserTextContinueTheTask(t *testing.T) {
	g, server := testGateway(t)
	var starts, results atomic.Int32
	g.run = fakeToolRunner(g, &starts, &results)
	req := toolRequest()
	_, first := postGateway(t, server, req, "")
	assistant := returnedMessage(t, first)
	// Claude Code appends reminders as a user message after tool results.
	req.Messages = append(req.Messages, assistant, apiMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: "tool-value"}, apiMessage{Role: "user", Content: "<system-reminder>keep going</system-reminder>"})
	code, answer := postGateway(t, server, req, "")
	text := textValue(returnedMessage(t, answer).Content)
	if code != 200 || starts.Load() != 1 || results.Load() != 1 || !strings.HasPrefix(text, "tool-value\n\n"+toolUserMarker) || !strings.HasSuffix(text, "keep going</system-reminder>") {
		t.Fatal("trailing user text broke the continuation", code, answer, starts.Load())
	}
	if usage := answer["usage"]; number(field(usage, "prompt_tokens")) == 0 || number(field(usage, "completion_tokens")) == 0 {
		t.Fatal("missing usage estimate", answer)
	}
}

func TestGatewayEvictsParkedTasksBeforeRefusingWork(t *testing.T) {
	g, server := testGateway(t)
	var starts, results atomic.Int32
	g.run = fakeToolRunner(g, &starts, &results)
	for i := 0; i < 5; i++ {
		req := toolRequest()
		req.Messages[0].Content = fmt.Sprint("task ", i)
		if code, answer := postGateway(t, server, req, ""); code != 200 || len(returnedMessage(t, answer).ToolCalls) != 1 {
			t.Fatal(i, code, answer)
		}
	}
	evicted := 0
	g.mu.Lock()
	for _, turn := range g.turns {
		if turn.abort != nil && turn.abort.Code == "task_evicted" {
			evicted++
		}
	}
	g.mu.Unlock()
	if starts.Load() != 5 || evicted != 1 {
		t.Fatal("expected the fifth task to evict one parked task", starts.Load(), evicted)
	}
}

func TestGatewayRefusesWorkWhenNoTaskIsParked(t *testing.T) {
	g, _ := testGateway(t)
	g.run = func(turn *gatewayTurn) { <-turn.ctx.Done(); g.finish(turn, "", nil) }
	ask := func(text string) *apiError {
		_, failure := g.submit(apiRequest{Model: "TestModel", Messages: []apiMessage{{Role: "user", Content: text}}}, "")
		return failure
	}
	for i := 0; i < 4; i++ {
		if failure := ask(fmt.Sprint("busy ", i)); failure != nil {
			t.Fatal(failure)
		}
	}
	if failure := ask("one more"); failure == nil || failure.Status != 429 || failure.Code != "gateway_capacity" {
		t.Fatal("busy gateway accepted work", failure)
	}
}

func TestGatewayOversizedPromptAsksForCompaction(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	g.run = func(turn *gatewayTurn) { starts.Add(1); g.finish(turn, "unexpected", nil) }
	req := apiRequest{Model: "TestModel", Messages: []apiMessage{{Role: "user", Content: strings.Repeat("word ", maxPromptChars/5+1)}}}
	code, answer := postGateway(t, server, req, "")
	// Claude Code compacts on this wording; OpenAI clients key on the code.
	pattern := regexp.MustCompile(`prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)
	m := pattern.FindStringSubmatch(textValue(field(answer, "error", "message")))
	if code != 400 || field(answer, "error", "code") != "context_length_exceeded" || m == nil || starts.Load() != 0 {
		t.Fatal("oversized prompt was not refused for compaction", code, answer)
	}
	if tokens, limit := number(m[1]), number(m[2]); limit >= tokens || limit == 0 {
		t.Fatal("limit must be below the estimate", tokens, limit)
	}
	req.Messages[0].Content = strings.Repeat("word ", 100)
	if code, _ := postGateway(t, server, req, ""); code != 200 || starts.Load() != 1 {
		t.Fatal("ordinary prompt refused", code)
	}
}

// Work offers connector tools only through tool_search, so the prompt lists
// the names Work gives them and asks for one search per full name.
func TestPromptNamesConnectorTools(t *testing.T) {
	req := toolRequest()
	req.Tools = append(req.Tools, apiTool{Type: "function", Function: apiFunction{Name: "Get-Weather", Parameters: object{"type": "object"}}})
	prompt := renderAPIPrompt(req, connectorName("0123456789abcdef"), false)
	for _, want := range []string{"完整工具名是：mcp__independent_http_tools_0123456789ab__client_nonce、mcp__independent_http_tools_0123456789ab__get_weather（Get-Weather）。", "query 只写这个函数的完整工具名，例如 mcp__independent_http_tools_0123456789ab__client_nonce；"} {
		if !strings.Contains(prompt, want) {
			t.Fatal("prompt lacks", want)
		}
	}
	parallel := true
	req.Parallel = &parallel
	if prompt = renderAPIPrompt(req, connectorName("0123456789abcdef"), false); !strings.Contains(prompt, "完整工具名是：mcp__independent_http_tools_0123456789ab__"+batchToolName+"。") {
		t.Fatal("batched prompt does not name the batch tool")
	}
}

// Work drops a connector tool whose description is too long, so the
// connector offers the start and the prompt carries the whole text.
func TestLongToolDescriptionsMoveIntoThePrompt(t *testing.T) {
	long := strings.Repeat("长", connectorDescriptionLimit) + "\nEND-OF-DOCS"
	payload := object{"model": "TestModel", "messages": []any{object{"role": "user", "content": "hi"}}, "tools": []any{
		object{"type": "function", "function": object{"name": "short", "description": "fits"}},
		object{"type": "function", "function": object{"name": "long", "description": long}},
	}}
	req, failure := parseAPIRequest(strings.NewReader(jsonText(payload)))
	if failure != nil {
		t.Fatal("long description refused", failure)
	}
	head := connectorDescription(req.Tools[1])
	if connectorDescription(req.Tools[0]) != "fits" || utf16Len(head) > 2100 || strings.Contains(head, "END-OF-DOCS") || !strings.Contains(head, "「函数 long 的完整说明」") {
		t.Fatal("connector description was not shortened", utf16Len(head))
	}
	if prompt := renderAPIPrompt(req, "connector", false); !strings.Contains(prompt, "「函数 long 的完整说明」\n"+long) || strings.Contains(prompt, "函数 short 的完整说明") {
		t.Fatal("prompt lacks exactly the long description")
	}
	if !strings.Contains(promptText(req), "END-OF-DOCS") || strings.Contains(jsonText(batchCatalog(req.Tools)), "END-OF-DOCS") {
		t.Fatal("the message limit must count the description and the batch must not carry it")
	}
	req.ToolChoice = "none"
	if strings.Contains(promptText(req), "END-OF-DOCS") {
		t.Fatal("tools that are not offered were documented")
	}
}

func TestLongToolSchemasMoveIntoThePrompt(t *testing.T) {
	big := object{"type": "object", "properties": object{"notes": object{"type": "string", "description": strings.Repeat("x", connectorToolLimit) + "END-OF-SCHEMA"}}, "required": []string{"notes"}}
	payload := object{"model": "TestModel", "messages": []any{object{"role": "user", "content": "hi"}}, "parallel_tool_calls": true, "tools": []any{
		object{"type": "function", "function": object{"name": "small", "description": "fits", "parameters": object{"type": "object", "properties": object{"q": object{"type": "string"}}}}},
		object{"type": "function", "function": object{"name": "big", "description": "takes notes", "parameters": big}},
	}}
	req, failure := parseAPIRequest(strings.NewReader(jsonText(payload)))
	if failure != nil {
		t.Fatal("long schema refused", failure)
	}
	if longSchema(req.Tools[0]) || jsonText(connectorSchema(req.Tools[0])) != jsonText(req.Tools[0].Function.Parameters) {
		t.Fatal("a small schema was replaced")
	}
	if schema := jsonText(connectorSchema(req.Tools[1])); strings.Contains(schema, "END-OF-SCHEMA") || !strings.Contains(schema, "「函数 big 的参数结构」") {
		t.Fatal("connector schema was not replaced", schema)
	}
	if prompt := renderAPIPrompt(req, "connector", false); !strings.Contains(prompt, "「函数 big 的参数结构」\n"+plainJSON(big)) || strings.Contains(prompt, "函数 small 的参数结构") || strings.Contains(prompt, batchToolName) {
		t.Fatal("prompt lacks exactly the long schema, or still batches")
	}
	// The limit counts the description too.
	req.Tools[0].Function.Description = strings.Repeat("y", connectorDescriptionLimit)
	req.Tools[0].Function.Parameters = object{"type": "object", "properties": object{"q": object{"type": "string", "description": strings.Repeat("z", connectorToolLimit-connectorDescriptionLimit)}}}
	if !longSchema(req.Tools[0]) || longDescription(req.Tools[0]) {
		t.Fatal("description and schema were not measured together")
	}
}

func TestPublicModelNamesAreStableSlugs(t *testing.T) {
	models := []Model{
		{ID: "1", Name: "自动", Display: "自动", Available: true, Modes: []string{"work"}},
		{ID: "2", Name: "豆包 2.1 Pro", Display: "豆包 2.1 Pro", Available: true, Modes: []string{"work"}},
		{ID: "3", Name: "GPT-6.1 Sol", Display: "GPT-6.1 Sol", Available: true, Modes: []string{"work"}},
		{ID: "4", Name: "GPT 6.1 Sol", Display: "GPT 6.1 Sol", Available: true, Modes: []string{"work"}},
		{ID: "5", Name: "Chat only", Display: "Chat only", Available: true, Modes: []string{"chat"}},
		{ID: "6", Name: "Offline", Display: "Offline", Modes: []string{"work"}},
	}
	got := []string{}
	for _, m := range publicModels(models) {
		got = append(got, m.Name+"="+m.Display)
	}
	want := "doubao-auto=自动 doubao-2.1-pro=豆包 2.1 Pro doubao-gpt-6.1-sol=GPT-6.1 Sol doubao-gpt-6.1-sol-4=GPT 6.1 Sol"
	if strings.Join(got, " ") != want {
		t.Fatal(got)
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
	for _, field := range []object{{"n": 2}, {"functions": []any{object{"name": "legacy"}}}} {
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

func TestConnectorEnableWaitsForTheListingToCatchUp(t *testing.T) {
	g, _ := testGateway(t)
	reads := 0
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reads++
		// Absent, then listed as disabled, then enabled.
		body := `{"code":0,"data":{"items":[],"has_more":false}}`
		if reads == 2 {
			body = `{"code":0,"data":{"items":[{"connector_id":"fixture-connector","enabled":false}],"has_more":false}}`
		} else if reads > 2 {
			body = `{"code":0,"data":{"items":[{"connector_id":"fixture-connector","enabled":true}],"has_more":false}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := g.c.awaitConnectorEnabled(context.Background(), "fixture-connector"); err != nil || reads != 3 {
		t.Fatal("connector was not awaited until enabled", err, reads)
	}
}

func TestConnectorEnableRetriesAStalledListing(t *testing.T) {
	defer func(wait, check time.Duration) { connectorEnableWait, connectorCheckTimeout = wait, check }(connectorEnableWait, connectorCheckTimeout)
	connectorEnableWait, connectorCheckTimeout = 2*time.Second, 50*time.Millisecond
	g, _ := testGateway(t)
	reads, stalls := 0, 1
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reads++
		if reads <= stalls {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body := `{"code":0,"data":{"items":[{"connector_id":"fixture-connector","enabled":true}],"has_more":false}}`
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := g.c.awaitConnectorEnabled(context.Background(), "fixture-connector"); err != nil || reads != 2 {
		t.Fatal("a stalled listing request used up the wait", err, reads)
	}
	reads, stalls = 0, 1<<20
	connectorEnableWait = 300 * time.Millisecond
	if err := g.c.awaitConnectorEnabled(context.Background(), "fixture-connector"); err == nil || !strings.Contains(err.Error(), "listing failed") {
		t.Fatal("a stalled listing was reported as a missing connector", err)
	}
}

func TestGatewayTaskThatNeverStartedIsSafeToRetry(t *testing.T) {
	defer func(wait time.Duration) { connectorEnableWait = wait }(connectorEnableWait)
	connectorEnableWait = time.Second
	g, server := testGateway(t)
	var disconnected atomic.Bool
	var generations atomic.Int32
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":0}`
		switch {
		case strings.HasSuffix(req.URL.Path, "/create_personal"):
			body = `{"code":0,"data":{"connector":{"connector_id":"fixture-connector"}}}`
		case strings.HasSuffix(req.URL.Path, "/disconnect"):
			disconnected.Store(true)
		case strings.HasSuffix(req.URL.Path, "/list_by_user"):
			body = `{"code":0,"data":{"items":[{"connector_id":"fixture-connector","enabled":false}],"has_more":false}}`
			if disconnected.Load() {
				body = `{"code":0,"data":{"items":[],"has_more":false}}`
			}
		default:
			generations.Add(1)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	status, response := postGateway(t, server, toolRequest(), "")
	if status != 429 || field(response, "error", "code") != "task_not_started" || !strings.Contains(textValue(field(response, "error", "message")), "listed as disabled") {
		t.Fatal("unstarted task did not ask for a retry", status, response)
	}
	if generations.Load() != 0 || !disconnected.Load() {
		t.Fatal("generation was submitted or connector left behind", generations.Load())
	}
}

func TestToolFramesReachTheirTaskOnAnyChannel(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newest, feed := io.Pipe()
	uploads := make(chan string, 4)
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/alice/office/tool_local/chunk_stream" {
			// Work sends every frame of the device over its newest channel, b's.
			var body io.ReadCloser = untilCancelled{req.Context()}
			if req.URL.Query().Get("sand_box_id") == "b" {
				body = newest
				go func() { <-req.Context().Done(); feed.CloseWithError(req.Context().Err()) }()
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
		}
		var body object
		if err := decode(req.Body, &body); err != nil {
			t.Error(err)
		}
		uploads <- textValue(body["sandbox_id"]) + ":" + textValue(field(body, "tool_result", "id"))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	release := make(chan struct{})
	forwarded := make(chan string, 4)
	serve := func(sandbox string) (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		p := &toolProbe{c: c, sandbox: sandbox, journal: filepath.Join(c.dir, "journal-"+sandbox+".json"), events: map[string]int{}, records: map[string]object{}, forward: func(ctx context.Context, call, _ object) (object, error) {
			forwarded <- sandbox + ":" + textValue(call["id"])
			if sandbox == "b" {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return object{"id": call["id"], "name": call["name"], "raw_result": `{"content":[],"isError":false}`}, nil
		}}
		return cancel, g.serveTools(ctx, ctx, cancel, p, nil)
	}
	cancelA, doneA := serve("a")
	defer cancelA()
	cancelB, doneB := serve("b")
	defer cancelB()
	frame := func(sandbox, id, input string) string {
		return "data: " + jsonText(object{"event_type": 1, "sandbox_id": sandbox, "tool_call": object{"id": id, "name": "connector.call", "input": input}}) + "\n\n"
	}
	next := func(values chan string) string {
		select {
		case value := <-values:
			return value
		case <-time.After(5 * time.Second):
			t.Fatal("a frame was not handled")
			return ""
		}
	}
	go io.WriteString(feed, frame("b", "b1", "{}")+frame("ended", "x1", "{}")+frame("a", "a1", "{}"))
	seen := map[string]bool{}
	for len(seen) < 2 {
		seen[next(forwarded)] = true
	}
	if !seen["a:a1"] || !seen["b:b1"] {
		t.Fatal("frames reached the wrong task", seen)
	}
	if got := next(uploads); got != "a:a1" {
		t.Fatal("a waited behind b's unresolved call", got)
	}
	close(release)
	if got := next(uploads); got != "b:b1" {
		t.Fatal(got)
	}
	// A reused call identity stops only the task that owns it.
	go io.WriteString(feed, frame("a", "a1", `{"changed":true}`))
	select {
	case err := <-doneA:
		if err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a handling failure did not stop its task")
	}
	select {
	case err := <-doneB:
		t.Fatal("another task's failure stopped b", err)
	default:
	}
	cancelB()
	<-doneB
	if len(forwarded) != 0 || len(uploads) != 0 {
		t.Fatal("handled a frame no running task owns")
	}
	g.inboxMu.Lock()
	defer g.inboxMu.Unlock()
	if len(g.inboxes) != 0 {
		t.Fatal("inboxes outlived their tasks")
	}
}
