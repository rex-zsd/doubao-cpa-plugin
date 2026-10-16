package doubao

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type apiFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  object `json:"parameters"`
	Strict      bool   `json:"strict,omitempty"`
}
type apiTool struct {
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}
type apiCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type apiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function apiCallFunction `json:"function"`
}
type apiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
}
type apiRequest struct {
	Model         string       `json:"model"`
	Messages      []apiMessage `json:"messages"`
	Tools         []apiTool    `json:"tools,omitempty"`
	ToolChoice    any          `json:"tool_choice,omitempty"`
	Parallel      *bool        `json:"parallel_tool_calls,omitempty"`
	Stream        bool         `json:"stream,omitempty"`
	N             int          `json:"n,omitempty"`
	StreamOptions object       `json:"stream_options,omitempty"`
	User          string       `json:"user,omitempty"`
}
type apiError struct {
	Status  int    `json:"-"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func apiFail(status int, code, message string) *apiError {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	return &apiError{Status: status, Message: message, Type: typ, Code: code}
}
func (e *apiError) Error() string { return e.Message }

var apiToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func apiContent(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, errors.New("content must be text or text parts")
	}
	texts := []string{}
	hasMedia := false
	for _, part := range parts {
		kind := textValue(field(part, "type"))
		if kind == "image_url" || kind == "file" {
			hasMedia = true
			continue
		}
		text, isText := field(part, "text").(string)
		if field(part, "type") != "text" || !isText {
			return nil, errors.New("only text content parts are supported")
		}
		texts = append(texts, text)
	}
	if hasMedia {
		return parts, nil
	}
	return strings.Join(texts, ""), nil
}

func parseAPIRequest(input io.Reader) (apiRequest, *apiError) {
	var req apiRequest
	data, err := io.ReadAll(io.LimitReader(input, (32<<20)+1))
	if err != nil || len(data) > 32<<20 {
		return req, apiFail(413, "request_too_large", "Request exceeds 32 MiB")
	}
	var raw object
	if err = decode(strings.NewReader(string(data)), &raw); err != nil || raw == nil {
		return req, apiFail(400, "invalid_json", "Expected one JSON object")
	}
	var rest any
	d := json.NewDecoder(strings.NewReader(string(data)))
	if d.Decode(&rest) != nil || d.Decode(&rest) != io.EOF {
		return req, apiFail(400, "invalid_json", "Expected one JSON object")
	}
	allowed := map[string]bool{"model": true, "messages": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true, "stream": true, "n": true, "stream_options": true, "user": true}
	for key, value := range raw {
		if !allowed[key] && value != nil {
			return req, apiFail(400, "unsupported_parameter", "Unsupported parameter: "+key)
		}
	}
	if err = decode(strings.NewReader(string(data)), &req); err != nil {
		return req, apiFail(400, "invalid_request", "Request fields have invalid types")
	}
	if req.Model == "" || len(req.Messages) == 0 || len(req.Messages) > 256 {
		return req, apiFail(400, "invalid_messages", "model and 1–256 text messages are required")
	}
	if req.N != 0 && req.N != 1 {
		return req, apiFail(400, "unsupported_n", "Only n=1 is supported")
	}
	if req.ToolChoice != nil && req.ToolChoice != "auto" && req.ToolChoice != "none" {
		return req, apiFail(400, "tool_choice_unsupported", "Only auto and none are supported; forced tool selection cannot be guaranteed")
	}
	for key, value := range req.StreamOptions {
		if key != "include_usage" || value != false && value != nil {
			return req, apiFail(400, "usage_unavailable", "Upstream token usage is unavailable; include_usage=true is unsupported")
		}
	}
	seen := map[string]bool{}
	if len(req.Tools) > 128 {
		return req, apiFail(400, "too_many_tools", "At most 128 functions are supported")
	}
	for i := range req.Tools {
		tool := &req.Tools[i]
		if tool.Type != "function" || !apiToolName.MatchString(tool.Function.Name) || seen[tool.Function.Name] {
			return req, apiFail(400, "invalid_tools", "Functions must have unique names of 1–64 letters, digits, underscores or hyphens")
		}
		seen[tool.Function.Name] = true
		if tool.Function.Strict {
			return req, apiFail(400, "strict_tools_unsupported", "Native strict JSON Schema enforcement is not available")
		}
		if tool.Function.Parameters == nil {
			tool.Function.Parameters = object{"type": "object", "properties": object{}}
		}
		if tool.Function.Parameters["type"] != "object" || len(jsonText(tool.Function.Parameters)) > 1<<20 || len(tool.Function.Description) > 20000 {
			return req, apiFail(400, "invalid_tool_schema", "Function parameters must be an object schema within 1 MiB")
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "system" && m.Role != "developer" && m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			return req, apiFail(400, "invalid_role", "Unsupported message role")
		}
		m.Content, err = apiContent(m.Content)
		if err != nil {
			return req, apiFail(400, "unsupported_content", err.Error())
		}
		if (m.Role == "user" || m.Role == "system" || m.Role == "developer" || m.Role == "tool") && m.Content == nil {
			return req, apiFail(400, "missing_content", "Text content is required for this message role")
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return req, apiFail(400, "missing_tool_call_id", "Tool messages require tool_call_id")
		}
		if len(m.ToolCalls) > 0 && m.Role != "assistant" {
			return req, apiFail(400, "invalid_tool_calls", "Only assistant messages may contain tool_calls")
		}
		if len(m.ToolCalls) > 0 && m.Content == "" {
			m.Content = nil
		}
		for j := range m.ToolCalls {
			var args any
			if err := decode(strings.NewReader(m.ToolCalls[j].Function.Arguments), &args); err != nil {
				return req, apiFail(400, "invalid_tool_arguments", "Tool arguments must be valid JSON")
			}
			m.ToolCalls[j].Function.Arguments = jsonText(args)
		}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" && last.Role != "tool" {
		return req, apiFail(400, "invalid_last_message", "The final message must be a user request or a tool result")
	}
	return req, nil
}

func apiHash(value any) string                { return fmt.Sprintf("%x", sha256.Sum256([]byte(jsonText(value)))) }
func apiMessagesEqual(a, b []apiMessage) bool { return jsonText(a) == jsonText(b) }

type apiExchange struct {
	mu      sync.Mutex
	visible string
	changed chan struct{}
	id      string
	model   string
	created int64
	ready   chan struct{}
	once    sync.Once
	message apiMessage
	finish  string
	err     *apiError
}

func newExchange(model string) *apiExchange {
	return &apiExchange{id: "chatcmpl_" + token(), model: model, created: time.Now().Unix(), ready: make(chan struct{}), changed: make(chan struct{})}
}
func (x *apiExchange) resolve(message apiMessage, finish string, err *apiError) {
	x.once.Do(func() {
		x.mu.Lock()
		if final, ok := message.Content.(string); ok && x.visible != "" && !strings.HasPrefix(final, x.visible) {
			err = apiFail(409, "visible_text_changed", "Upstream revised text already streamed; final answer cannot be appended safely")
		}
		x.message, x.finish, x.err = message, finish, err
		x.mu.Unlock()
		close(x.ready)
	})
}

type clientToolPending struct {
	nativeID string
	exposed  *apiExchange

	turn       *gatewayTurn
	call       apiToolCall
	batchCalls []apiToolCall
	expected   []apiMessage
	result     chan string
	submitted  bool
	value      string
	next       *apiExchange
}
type gatewayTurn struct {
	initialKey, initialHash string
	first                   *apiExchange
	finalText               string

	id                                         string
	request                                    apiRequest
	current                                    *apiExchange
	ctx                                        context.Context
	cancel                                     context.CancelFunc
	statePath, receiptPath, connector, sandbox string
	done                                       bool
	finished                                   time.Time
	deadline                                   time.Time
	err                                        *apiError
	generationPosts                            int
	forwarded                                  int
	denied                                     int
	cleanupConfirmed                           bool
	cleanupRequested                           bool
}
type gatewayIdempotency struct {
	hash     string
	turn     *gatewayTurn
	exchange *apiExchange
}
type gateway struct {
	suspending  bool
	helperPath  string
	c           *Client
	models      []Model
	dir         string
	taskTimeout time.Duration
	ctx         context.Context
	apiKey      string
	mu          sync.Mutex
	turns       map[string]*gatewayTurn
	calls       map[string]*clientToolPending
	idempotency map[string]gatewayIdempotency
	wg          sync.WaitGroup
	// Tests substitute only the upstream task runner, exercising real HTTP/state code.
	run func(*gatewayTurn)
}

func newGateway(ctx context.Context, c *Client, models []Model, dir string, timeout time.Duration) *gateway {
	g := &gateway{c: c, models: models, dir: dir, ctx: ctx, taskTimeout: timeout, apiKey: os.Getenv("DOUBAO_PROXY_API_KEY"), turns: map[string]*gatewayTurn{}, calls: map[string]*clientToolPending{}, idempotency: map[string]gatewayIdempotency{}}
	g.run = g.runNative
	return g
}
func (g *gateway) checkpointLocked(t *gatewayTurn) error {
	pending := []any{}
	for id, p := range g.calls {
		if p.turn == t {
			pending = append(pending, object{"id": id, "call": p.call, "native_id": p.nativeID, "exchange": exchangeRecord(p.exposed), "next_exchange": exchangeRecord(p.next), "batch_calls": p.batchCalls, "expected_messages": p.expected, "submitted": p.submitted, "result": p.value})
		}
	}
	return saveJSON(t.statePath, object{"version": 2, "finished_at": t.finished.Unix(), "deadline": t.deadline.Unix(), "error_status": errorStatus(t.err), "id": t.id, "initial_key": t.initialKey, "initial_hash": t.initialHash, "first_exchange": exchangeRecord(t.first), "current_exchange": exchangeRecord(t.current), "final_text": t.finalText, "request": t.request, "connector_id": t.connector, "sandbox_id": t.sandbox, "receipt_path": t.receiptPath, "generation_posts": t.generationPosts, "forwarded_tools": t.forwarded, "denied_tools": t.denied, "done": t.done, "cleanup_requested": t.cleanupRequested, "cleanup_confirmed": t.cleanupConfirmed, "pending": pending, "error": t.err})
}
func (g *gateway) sweepLocked() {
	for id, t := range g.turns {
		if t.done && (t.connector == "" || t.cleanupConfirmed) && time.Since(t.finished) > 30*time.Minute {
			for _, prefix := range []string{"gateway-state-", "gateway-native-", "gateway-schema-", "gateway-receipt-"} {
				_ = os.Remove(filepath.Join(g.dir, prefix+id+".json"))
			}
			_ = os.RemoveAll(filepath.Join(g.dir, "gateway-workspace-"+id))
			_ = os.Remove(filepath.Join(g.dir, "gateway-receipt-"+id+".json.stop.json"))
			mediaReceipts, _ := filepath.Glob(filepath.Join(g.dir, "gateway-media-"+id+"-*.json"))
			for _, path := range mediaReceipts {
				_ = os.Remove(path)
				_ = os.Remove(path + ".stop.json")
			}
			delete(g.turns, id)
			for call, p := range g.calls {
				if p.turn == t {
					delete(g.calls, call)
				}
			}
			for key, entry := range g.idempotency {
				if entry.turn == t {
					delete(g.idempotency, key)
				}
			}
		}
	}
}

func (g *gateway) submit(req apiRequest, key string) (*apiExchange, *apiError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return nil, apiFail(503, "gateway_shutting_down", "Gateway is shutting down")
	}
	g.sweepLocked()
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		p := g.calls[last.ToolCallID]
		if p == nil {
			return nil, apiFail(409, "unknown_tool_call", "Unknown or expired tool_call_id; no new native task was created")
		}
		t := p.turn
		if req.Model != t.request.Model || req.Tools != nil && jsonText(req.Tools) != jsonText(t.request.Tools) || req.ToolChoice != nil && req.ToolChoice != t.request.ToolChoice {
			return nil, apiFail(409, "tool_context_mismatch", "Keep the original model and tool definitions when returning a tool result")
		}
		if req.Parallel != nil && parallelRequested(req) != parallelRequested(t.request) {
			return nil, apiFail(409, "tool_context_mismatch", "Keep parallel_tool_calls unchanged during a tool continuation")
		}
		if len(p.batchCalls) > 0 {
			return g.submitBatchLocked(req, p)
		}
		if len(req.Messages) != len(p.expected)+1 || !apiMessagesEqual(req.Messages[:len(req.Messages)-1], p.expected) {
			return nil, apiFail(409, "tool_history_mismatch", "Tool continuation must include the exact returned assistant message and its prior history")
		}
		value := apiResultText(last.Content)
		if p.submitted {
			if value != p.value {
				return nil, apiFail(409, "tool_result_conflict", "A different result has already been supplied for this tool_call_id")
			}
			return p.next, nil
		}
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
		req.Tools = t.request.Tools
		req.ToolChoice = t.request.ToolChoice
		req.Parallel = t.request.Parallel
		t.request = req
		if err := g.checkpointLocked(t); err != nil {
			p.submitted, p.value, p.next = false, "", nil
			t.current, t.request = previousExchange, previousRequest
			return nil, apiFail(500, "state_persistence_failed", "Could not persist tool continuation; nothing was uploaded")
		}
		p.result <- value
		return x, nil
	}
	if len(key) > 128 {
		return nil, apiFail(400, "invalid_idempotency_key", "Idempotency-Key exceeds 128 characters")
	}
	fingerprint := apiHash(object{"model": req.Model, "messages": req.Messages, "tools": req.Tools, "tool_choice": req.ToolChoice, "parallel": parallelRequested(req)})
	if key != "" {
		if prior, ok := g.idempotency[apiHash(key)]; ok {
			if prior.hash != fingerprint {
				return nil, apiFail(409, "idempotency_conflict", "Idempotency-Key was already used with a different request")
			}
			return prior.exchange, nil
		}
	}
	active := 0
	for _, t := range g.turns {
		if !t.done {
			active++
		}
	}
	if active >= 4 || len(g.turns) >= 128 {
		return nil, apiFail(429, "gateway_capacity", "Gateway capacity reached; wait for existing tasks to finish")
	}
	deadline := time.Now().Add(g.taskTimeout)
	ctx, cancel := context.WithDeadline(g.ctx, deadline)
	x := newExchange(req.Model)
	id := token()
	t := &gatewayTurn{id: id, deadline: deadline, initialHash: fingerprint, first: x, request: req, current: x, ctx: ctx, cancel: cancel, statePath: filepath.Join(g.dir, "gateway-state-"+id+".json"), receiptPath: filepath.Join(g.dir, "gateway-receipt-"+id+".json")}
	if key != "" {
		t.initialKey = apiHash(key)
	}
	if err := g.checkpointLocked(t); err != nil {
		cancel()
		return nil, apiFail(500, "state_persistence_failed", "Could not persist request; generation was not submitted")
	}
	g.turns[id] = t
	if key != "" {
		g.idempotency[apiHash(key)] = gatewayIdempotency{hash: fingerprint, turn: t, exchange: x}
	}
	g.wg.Add(1)
	go func() { defer g.wg.Done(); g.run(t) }()
	return x, nil
}

func (g *gateway) finish(t *gatewayTurn, text string, failure *apiError) {
	g.mu.Lock()
	t.done, t.finished, t.err, t.finalText = true, time.Now(), failure, text
	x := t.current
	if err := g.checkpointLocked(t); err != nil && failure == nil {
		failure = apiFail(500, "state_persistence_failed", "Upstream completed but its final state could not be persisted; do not resubmit generation")
		t.err = failure
	}
	g.mu.Unlock()
	t.cancel()
	x.resolve(apiMessage{Role: "assistant", Content: text}, "stop", failure)
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(object{"error": e})
}
func apiCompletion(x *apiExchange) object {
	return object{"id": x.id, "object": "chat.completion", "created": x.created, "model": x.model, "choices": []any{object{"index": 0, "message": x.message, "finish_reason": x.finish, "logprobs": nil}}}
}

func writeAPIExchange(w http.ResponseWriter, r *http.Request, x *apiExchange, stream bool) {
	if !stream {
		select {
		case <-x.ready:
		case <-r.Context().Done():
			return
		}
		if x.err != nil {
			writeAPIError(w, x.err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiCompletion(x))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flush, _ := w.(http.Flusher)
	emit := func(value any) {
		fmt.Fprintf(w, "data: %s\n\n", jsonText(value))
		if flush != nil {
			flush.Flush()
		}
	}
	chunk := func(delta object, finish any) object {
		return object{"id": x.id, "object": "chat.completion.chunk", "created": x.created, "model": x.model, "choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}}}
	}
	emit(chunk(object{"role": "assistant"}, nil))
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	sent := ""
	for {
		visible, changed := x.visibleSnapshot()
		if strings.HasPrefix(visible, sent) && len(visible) > len(sent) {
			emit(chunk(object{"content": visible[len(sent):]}, nil))
			sent = visible
		}
		select {
		case <-changed:
			continue
		case <-x.ready:
			if x.err != nil {
				emit(object{"error": x.err})
			} else if len(x.message.ToolCalls) > 0 {
				calls := []any{}
				for i, call := range x.message.ToolCalls {
					calls = append(calls, object{"index": i, "id": call.ID, "type": "function", "function": call.Function})
				}
				emit(chunk(object{"tool_calls": calls}, nil))
				emit(chunk(object{}, x.finish))
			} else {
				final := textValue(x.message.Content)
				if strings.HasPrefix(final, sent) && len(final) > len(sent) {
					emit(chunk(object{"content": final[len(sent):]}, nil))
				}
				emit(chunk(object{}, x.finish))
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flush != nil {
				flush.Flush()
			}
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			if flush != nil {
				flush.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
		return
	}
	if g.apiKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.apiKey)) != 1 {
		writeAPIError(w, apiFail(401, "invalid_api_key", "Invalid API key"))
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == "GET" {
		items := []any{}
		for _, m := range g.models {
			if m.Available && contains(m.Modes, "work") {
				items = append(items, object{"id": m.Name, "object": "model", "created": 0, "owned_by": "doubao-work"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(object{"object": "list", "data": items})
		return
	}
	if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" {
		writeAPIError(w, apiFail(404, "not_found", "Use GET /v1/models or POST /v1/chat/completions"))
		return
	}
	req, failure := parseAPIRequest(r.Body)
	if failure != nil {
		writeAPIError(w, failure)
		return
	}
	if _, _, err := selectModel(g.models, req.Model, "work"); err != nil {
		writeAPIError(w, apiFail(400, "model_not_found", "Choose an available model name from /v1/models"))
		return
	}
	x, failure := g.submit(req, r.Header.Get("Idempotency-Key"))
	if failure != nil {
		writeAPIError(w, failure)
		return
	}
	writeAPIExchange(w, r, x, req.Stream)
}

func serveGateway(c *Client, dir, address string, timeout time.Duration) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("gateway listen address must be an explicit loopback IP and port")
	}
	if timeout < 30*time.Second || timeout > 30*time.Minute {
		return errors.New("task-timeout must be between 30s and 30m")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = c.account(startup); err != nil {
		return err
	}
	models, err := c.models(startup)
	if err != nil {
		return err
	}
	g := newGateway(ctx, c, models, dir, timeout)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	fmt.Printf("GATEWAY_LISTENING http://%s/v1 models=%d authentication=%t\n", listener.Addr().String(), len(models), g.apiKey != "")
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		done := make(chan struct{})
		go func() { g.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(45 * time.Second):
			fmt.Fprintln(os.Stderr, "GATEWAY_SHUTDOWN_CLEANUP_INCOMPLETE")
		}
		return nil
	}
	return err
}

func errorStatus(e *apiError) int {
	if e == nil {
		return 0
	}
	return e.Status
}
