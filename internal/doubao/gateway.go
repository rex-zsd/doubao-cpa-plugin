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
	// Effort is Work's numeric reasoning level (see apiEffort); zero keeps the
	// model default.
	Effort         int    `json:"reasoning_effort,omitempty"`
	ResponseFormat object `json:"response_format,omitempty"`
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

// Work takes each request as one message of at most this many UTF-16 code
// units; longer ones fail with error 710020202 (measured 2026-10).
const maxPromptChars = 262144

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
	texts, normalized := []string{}, []any{}
	hasMedia := false
	for _, part := range parts {
		kind := textValue(field(part, "type"))
		if kind == "image_url" || kind == "file" {
			hasMedia = true
			normalized = append(normalized, part)
			continue
		}
		text, isText := field(part, "text").(string)
		switch kind {
		case "text", "input_text", "output_text":
		case "refusal":
			text, isText = field(part, "refusal").(string)
		default:
			isText = false
		}
		if !isText {
			return nil, errors.New("only text, image and file content parts are supported")
		}
		texts = append(texts, text)
		normalized = append(normalized, object{"type": "text", "text": text})
	}
	if hasMedia {
		return normalized, nil
	}
	return strings.Join(texts, "\n"), nil
}

// apiEffort maps reasoning effort names onto Work's levels: catalog options
// are 3–5 (低/中/高) for 豆包 models and 3–7 for the others. Numbers pass
// through; auto, default and unknown names keep the model default.
func apiEffort(v any) int {
	text := strings.ToLower(strings.TrimSpace(textValue(v)))
	switch text {
	case "none", "minimal", "low":
		return 3
	case "medium":
		return 4
	case "high":
		return 5
	case "xhigh":
		return 6
	case "max":
		return 7
	}
	if n := number(v); n > 0 && n <= 100 {
		return n
	}
	return 0
}

// apiToolChoice normalizes OpenAI, Responses and Anthropic tool_choice forms.
// Work cannot force a function call, so "required" and named functions become
// prompt instructions; unrecognized objects fall back to auto (nil).
func apiToolChoice(v any, tools []apiTool) (any, *apiError) {
	if v == nil || len(tools) == 0 {
		return nil, nil
	}
	choice, name := textValue(v), ""
	if m, ok := v.(map[string]any); ok {
		choice = textValue(m["type"])
		name = textValue(field(m, "function", "name"))
		if name == "" {
			name = textValue(m["name"])
		}
	}
	switch choice {
	case "auto":
		return nil, nil
	case "none":
		return "none", nil
	case "required", "any":
		return "required", nil
	case "function", "tool", "custom":
		for _, tool := range tools {
			if tool.Function.Name == name {
				return object{"type": "function", "function": object{"name": name}}, nil
			}
		}
		return nil, apiFail(400, "invalid_tool_choice", "tool_choice names a function that is not in tools")
	}
	if _, ok := v.(string); ok {
		return nil, apiFail(400, "invalid_tool_choice", "tool_choice must be auto, none, required or a declared function")
	}
	return nil, nil
}

// apiResponseFormat keeps JSON modes, which are enforced by instruction only.
func apiResponseFormat(v any) (object, *apiError) {
	if v == nil {
		return nil, nil
	}
	format, ok := v.(map[string]any)
	if !ok {
		return nil, apiFail(400, "invalid_response_format", "response_format must be an object")
	}
	switch format["type"] {
	case "json_object":
		return object{"type": "json_object"}, nil
	case "json_schema":
		return object{"type": "json_schema", "json_schema": format["json_schema"]}, nil
	}
	return nil, nil
}
func jsonMode(req apiRequest) bool { return req.ResponseFormat != nil }

// usesTools reports whether the turn offers caller functions to the model.
func usesTools(req apiRequest) bool { return len(req.Tools) > 0 && req.ToolChoice != "none" }

// normalizeSchemaTypes lower-cases the OpenAPI type names that Gemini clients
// send (OBJECT, STRING, ...) so that the connector receives JSON Schema.
func normalizeSchemaTypes(schema any) {
	s, ok := schema.(object)
	if !ok {
		return
	}
	name := func(text string) string {
		switch lower := strings.ToLower(text); lower {
		case "object", "string", "number", "integer", "boolean", "array", "null":
			return lower
		}
		return text
	}
	switch t := s["type"].(type) {
	case string:
		s["type"] = name(t)
	case []any:
		for i, v := range t {
			if text, ok := v.(string); ok {
				t[i] = name(text)
			}
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions"} {
		if children, ok := s[key].(object); ok {
			for _, child := range children {
				normalizeSchemaTypes(child)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties", "not", "anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := s[key].([]any); ok {
			for _, child := range list {
				normalizeSchemaTypes(child)
			}
		} else {
			normalizeSchemaTypes(s[key])
		}
	}
}

func parseAPIRequest(input io.Reader) (apiRequest, *apiError) {
	var req apiRequest
	var failure *apiError
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
	// Work exposes no sampling, length, stop or seed controls (measured
	// 2026-10), so such parameters are accepted and ignored. Only requests whose
	// answer would silently differ from what was asked for are rejected.
	if truthy(raw["functions"]) || truthy(raw["function_call"]) {
		return req, apiFail(400, "unsupported_parameter", "Legacy functions/function_call are unsupported; use tools")
	}
	if truthy(raw["audio"]) || strings.Contains(jsonText(raw["modalities"]), "audio") {
		return req, apiFail(400, "unsupported_parameter", "Audio output is unsupported")
	}
	known := object{}
	for _, key := range []string{"model", "messages", "tools", "parallel_tool_calls", "stream", "n", "stream_options"} {
		if raw[key] != nil {
			known[key] = raw[key]
		}
	}
	if user, ok := raw["user"].(string); ok {
		known["user"] = user
	}
	if err = decode(strings.NewReader(jsonText(known)), &req); err != nil {
		return req, apiFail(400, "invalid_request", "Request fields have invalid types")
	}
	if req.Model == "" || len(req.Messages) == 0 {
		return req, apiFail(400, "invalid_messages", "model and at least one message are required")
	}
	if req.N != 0 && req.N != 1 {
		return req, apiFail(400, "unsupported_n", "Only n=1 is supported")
	}
	effort := raw["reasoning_effort"]
	if effort == nil {
		effort = field(raw, "reasoning", "effort")
	}
	req.Effort = apiEffort(effort)
	if req.ResponseFormat, failure = apiResponseFormat(raw["response_format"]); failure != nil {
		return req, failure
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
		// strict is best effort: the connector forwards the schema as given.
		tool.Function.Strict = false
		if tool.Function.Parameters == nil {
			tool.Function.Parameters = object{"type": "object", "properties": object{}}
		}
		normalizeSchemaTypes(tool.Function.Parameters)
		// Long descriptions and schemas are not refused: see connectorDescription
		// and connectorSchema.
		if tool.Function.Parameters["type"] != "object" || len(jsonText(tool.Function.Parameters)) > 1<<20 {
			return req, apiFail(400, "invalid_tool_schema", "Function parameters must be a JSON Schema object of at most 1 MiB")
		}
	}
	if req.ToolChoice, failure = apiToolChoice(raw["tool_choice"], req.Tools); failure != nil {
		return req, failure
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
	// prompt is the estimated prompt size reported as usage; Work reports none.
	prompt  int
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
	// abort is set when a newer request superseded or evicted the task; the
	// runner stops upstream work and finishes with it.
	abort            *apiError
	generationPosts  int
	forwarded        int
	denied           int
	cleanupConfirmed bool
	cleanupRequested bool
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
	// inboxes queues native tool frames for each running task by sandbox.
	inboxMu sync.Mutex
	inboxes map[string]toolInbox
	wg      sync.WaitGroup
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
	for _, t := range g.turns {
		if t.done && (t.connector == "" || t.cleanupConfirmed) && time.Since(t.finished) > 30*time.Minute {
			g.dropTurnLocked(t)
		}
	}
}

// dropTurnLocked forgets a finished task and deletes its private files.
func (g *gateway) dropTurnLocked(t *gatewayTurn) {
	id := t.id
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

func (g *gateway) submit(req apiRequest, key string) (*apiExchange, *apiError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return nil, apiFail(503, "gateway_shutting_down", "Gateway is shutting down")
	}
	g.sweepLocked()
	if len(key) > 128 {
		return nil, apiFail(400, "invalid_idempotency_key", "Idempotency-Key exceeds 128 characters")
	}
	head, results, after := splitContinuation(req.Messages)
	if len(results) > 0 {
		return g.continueLocked(req, key, head, results, after)
	}
	return g.startLocked(req, key, false)
}

// splitContinuation finds the tool results that end a conversation. Callers
// such as Claude Code follow them with user text (reminders, a new
// instruction), which stays part of the continuation.
func splitContinuation(messages []apiMessage) (head, results, after []apiMessage) {
	end := len(messages)
	for end > 0 && messages[end-1].Role == "user" {
		end--
	}
	start := end
	for start > 0 && messages[start-1].Role == "tool" {
		start--
	}
	return messages[:start], messages[start:end], messages[end:]
}

// continueLocked hands caller tool results to the native task waiting for
// them. A request the task cannot continue (edited history, different
// results, changed tools, or a task that already ended) replaces it with a
// new task over the full transcript instead of failing.
func (g *gateway) continueLocked(req apiRequest, key string, head, results, after []apiMessage) (*apiExchange, *apiError) {
	var p *clientToolPending
	for _, m := range results {
		if p = g.calls[m.ToolCallID]; p != nil {
			break
		}
	}
	if p == nil {
		if n := len(head); n > 0 && head[n-1].Role == "assistant" && len(head[n-1].ToolCalls) > 0 {
			return g.startLocked(req, key, true)
		}
		return nil, apiFail(409, "unknown_tool_call", "Unknown or expired tool_call_id; no new native task was created")
	}
	t := p.turn
	restart := func() (*apiExchange, *apiError) {
		g.supersedeLocked(t, apiFail(409, "task_superseded", "A newer request replaced this task's conversation"))
		return g.startLocked(req, key, true)
	}
	changed := req.Model != t.request.Model || req.Tools != nil && jsonText(req.Tools) != jsonText(t.request.Tools) || req.ToolChoice != nil && jsonText(req.ToolChoice) != jsonText(t.request.ToolChoice) || req.Parallel != nil && parallelRequested(req) != parallelRequested(t.request)
	if changed || !apiMessagesEqual(head, p.expected) || len(p.batchCalls) == 0 && (len(results) != 1 || results[0].ToolCallID != p.call.ID) {
		return restart()
	}
	extra := []string{}
	for _, m := range after {
		if _, media := m.Content.([]any); media {
			return restart()
		}
		extra = append(extra, textValue(m.Content))
	}
	value, failure := continuationValue(p, results, strings.Join(extra, "\n\n"))
	if failure != nil {
		return nil, failure
	}
	if p.submitted {
		if value != p.value || failedLocked(t, p.next) {
			return restart()
		}
		return p.next, nil
	}
	if t.done || t.abort != nil {
		return restart()
	}
	x := newExchange(req.Model)
	x.prompt = estimateTokens(promptText(req))
	previousExchange, previousRequest := t.current, t.request
	p.submitted, p.value, p.next = true, value, x
	t.current = x
	req.Tools, req.ToolChoice, req.Parallel = t.request.Tools, t.request.ToolChoice, t.request.Parallel
	req.Effort, req.ResponseFormat = t.request.Effort, t.request.ResponseFormat
	t.request = req
	if err := g.checkpointLocked(t); err != nil {
		p.submitted, p.value, p.next = false, "", nil
		t.current, t.request = previousExchange, previousRequest
		return nil, apiFail(500, "state_persistence_failed", "Could not persist tool continuation; nothing was uploaded")
	}
	p.result <- value
	return x, nil
}

// toolUserMarker introduces user text that followed the tool results, so the
// model can tell it from the function output.
const toolUserMarker = "[调用方在工具结果后追加的用户消息]\n"

// continuationValue encodes the results for one pending call or batch. A
// batch is accepted atomically, in any order, and never uploaded partially.
func continuationValue(p *clientToolPending, results []apiMessage, extra string) (string, *apiError) {
	if len(p.batchCalls) == 0 {
		value := apiResultText(results[0].Content)
		if parts, ok := results[0].Content.([]any); ok && extra != "" {
			parts = append(append([]any{}, parts...), object{"type": "text", "text": toolUserMarker + extra, "user_message": true})
			value = jsonText(parts)
		} else if extra != "" {
			value += "\n\n" + toolUserMarker + extra
		}
		return value, nil
	}
	if len(results) != len(p.batchCalls) {
		return "", apiFail(400, "incomplete_tool_batch", "Supply one tool result for every call in this batch in one request")
	}
	wanted, values := map[string]bool{}, map[string]string{}
	for _, call := range p.batchCalls {
		wanted[call.ID] = true
	}
	for _, message := range results {
		if _, duplicate := values[message.ToolCallID]; !wanted[message.ToolCallID] || duplicate {
			return "", apiFail(409, "invalid_tool_batch", "Batch contains unknown, duplicate or non-tool results")
		}
		values[message.ToolCallID] = apiResultText(message.Content)
	}
	rows := []any{}
	for i, call := range p.batchCalls {
		rows = append(rows, object{"index": i, "name": call.Function.Name, "result": values[call.ID]})
	}
	value := object{"results": rows}
	if extra != "" {
		value["user_message"] = toolUserMarker + extra
	}
	return jsonText(value), nil
}

// failedLocked reports an exchange that can no longer produce an answer, so
// an identical retry starts over instead of replaying it.
func failedLocked(t *gatewayTurn, x *apiExchange) bool {
	if x == nil {
		return true
	}
	select {
	case <-x.ready:
		return x.err != nil
	default:
		return t.abort != nil
	}
}

// supersedeLocked stops a task whose conversation moved on without it.
func (g *gateway) supersedeLocked(t *gatewayTurn, abort *apiError) {
	if t.done || t.abort != nil {
		return
	}
	t.abort = abort
	t.cancel()
}

// apiFingerprint identifies a request for idempotency. Optional fields join
// only when set, so fingerprints saved by earlier versions still match.
func apiFingerprint(req apiRequest) string {
	value := object{"model": req.Model, "messages": req.Messages, "tools": req.Tools, "tool_choice": req.ToolChoice, "parallel": parallelRequested(req)}
	if req.Effort != 0 {
		value["reasoning_effort"] = req.Effort
	}
	if req.ResponseFormat != nil {
		value["response_format"] = req.ResponseFormat
	}
	return apiHash(value)
}

// startLocked creates a native task. transcript marks a conversation whose
// tool results no live task can take; identical retries of it share one task
// even without an Idempotency-Key.
func (g *gateway) startLocked(req apiRequest, key string, transcript bool) (*apiExchange, *apiError) {
	fingerprint := apiFingerprint(req)
	if key != "" {
		if prior, ok := g.idempotency[apiHash(key)]; ok {
			if prior.hash != fingerprint {
				return nil, apiFail(409, "idempotency_conflict", "Idempotency-Key was already used with a different request")
			}
			return prior.exchange, nil
		}
	}
	implicit := ""
	if transcript && key == "" {
		implicit = apiHash("transcript\x00" + fingerprint)
		if prior, ok := g.idempotency[implicit]; ok {
			if !failedLocked(prior.turn, prior.exchange) {
				return prior.exchange, nil
			}
			delete(g.idempotency, implicit)
		}
	}
	prompt := promptText(req)
	if utf16Len(prompt) > maxPromptChars {
		return nil, promptTooLong(prompt)
	}
	if failure := g.reserveLocked(); failure != nil {
		return nil, failure
	}
	deadline := time.Now().Add(g.taskTimeout)
	ctx, cancel := context.WithDeadline(g.ctx, deadline)
	x := newExchange(req.Model)
	x.prompt = estimateTokens(prompt)
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
	if implicit != "" {
		g.idempotency[implicit] = gatewayIdempotency{hash: fingerprint, turn: t, exchange: x}
	}
	g.wg.Add(1)
	go func() { defer g.wg.Done(); g.run(t) }()
	return x, nil
}

// reserveLocked makes room for one more task. A task parked on a caller tool
// result holds no upstream generation, so the one closest to its deadline is
// evicted before new work is refused. Refusal is 429: nothing was submitted,
// so retrying is safe, and 429 is the one 4xx every client retries (Codex
// never retries 409). The CPA plugin's auth turns off cooldown, so the
// refusal does not bench the model.
func (g *gateway) reserveLocked() *apiError {
	full := apiFail(429, "gateway_capacity", "Gateway capacity reached; wait for existing tasks to finish")
	for len(g.turns) >= 128 {
		var oldest *gatewayTurn
		for _, t := range g.turns {
			if t.done && (t.connector == "" || t.cleanupConfirmed) && (oldest == nil || t.finished.Before(oldest.finished)) {
				oldest = t
			}
		}
		if oldest == nil {
			return full
		}
		g.dropTurnLocked(oldest)
	}
	active := 0
	var parked *gatewayTurn
	for _, t := range g.turns {
		if t.done || t.abort != nil {
			continue
		}
		active++
		if g.waitingLocked(t) && (parked == nil || t.deadline.Before(parked.deadline)) {
			parked = t
		}
	}
	if active < 4 {
		return nil
	}
	if parked == nil {
		return full
	}
	g.supersedeLocked(parked, apiFail(409, "task_evicted", "The task was evicted while waiting for a tool result; resend the conversation to start a new task"))
	return nil
}

// waitingLocked reports a task parked on a caller tool result.
func (g *gateway) waitingLocked(t *gatewayTurn) bool {
	for _, p := range g.calls {
		if p.turn == t && !p.submitted {
			return true
		}
	}
	return false
}

// promptText renders the prompt a task sends, with placeholders for media and
// the connector name, so Work's size limit is checked before any upload.
func promptText(req apiRequest) string {
	clone := req
	clone.Messages = append([]apiMessage{}, req.Messages...)
	attachments := 0
	for i, m := range clone.Messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		text := []string{}
		for _, part := range parts {
			if field(part, "type") == "text" {
				text = append(text, textValue(field(part, "text")))
				continue
			}
			attachments++
			text = append(text, mediaPlaceholder(attachments, mediaLabel(part), i+1, m.Role))
		}
		clone.Messages[i].Content = strings.Join(text, "\n")
	}
	connector := ""
	if usesTools(req) {
		connector = connectorName(strings.Repeat("0", 32))
	}
	return renderAPIPrompt(clone, connector)
}

// utf16Len counts UTF-16 code units, the unit of Work's message limit.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// estimateTokens approximates tokenizer counts: about four ASCII characters
// or one other character per token.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// promptTooLong uses the wording clients parse to trigger compaction:
// "prompt is too long: N tokens > M maximum". Work limits characters, so M is
// that limit scaled by this prompt's characters per estimated token.
func promptTooLong(text string) *apiError {
	chars, tokens := utf16Len(text), estimateTokens(text)
	limit := tokens * maxPromptChars / max(chars, 1)
	detail := fmt.Sprintf("this request renders to %d", chars)
	if limit >= tokens {
		limit, detail = tokens*9/10, "Work measured this one as longer"
	}
	return apiFail(400, "context_length_exceeded", fmt.Sprintf("prompt is too long: %d tokens > %d maximum (Doubao Work takes at most %d characters per message; %s)", tokens, limit, maxPromptChars, detail))
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
	return object{"id": x.id, "object": "chat.completion", "created": x.created, "model": x.model, "choices": []any{object{"index": 0, "message": x.message, "finish_reason": x.finish, "logprobs": nil}}, "usage": apiUsage(x)}
}

// apiUsage estimates token usage; Work reports none. Clients size their
// context from prompt_tokens, so it is better than reporting zero.
func apiUsage(x *apiExchange) object {
	completion := estimateTokens(textValue(x.message.Content))
	if len(x.message.ToolCalls) > 0 {
		completion += estimateTokens(jsonText(x.message.ToolCalls))
	}
	return object{"prompt_tokens": x.prompt, "completion_tokens": completion, "total_tokens": x.prompt + completion}
}

var jsonFence = regexp.MustCompile("(?s)^```(?:json)?\\s*\\n(.*?)\\n?```$")

// unwrapJSONFence strips the Markdown fence Work tends to put around JSON
// answers. Anything that is not valid JSON once unwrapped is left as is.
func unwrapJSONFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if m := jsonFence.FindStringSubmatch(trimmed); m != nil {
		trimmed = strings.TrimSpace(m[1])
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}
	return text
}

func writeAPIExchange(w http.ResponseWriter, r *http.Request, x *apiExchange, stream, includeUsage bool) {
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
	emit := func(frame string) error {
		_, err := io.WriteString(w, frame)
		if flush != nil {
			flush.Flush()
		}
		return err
	}
	err := streamExchange(r.Context(), emit, x, includeUsage)
	var failure *apiError
	if errors.As(err, &failure) {
		err = emit("data: " + jsonText(object{"error": failure}) + "\n\n")
	}
	if err == nil {
		emit("data: [DONE]\n\n")
	}
}

// streamExchange writes x as chat.completion.chunk SSE frames. It returns x's
// failure, if any, for the caller to report, and never writes [DONE].
func streamExchange(ctx context.Context, emit func(string) error, x *apiExchange, includeUsage bool) error {
	chunk := func(delta object, finish any) object {
		return object{"id": x.id, "object": "chat.completion.chunk", "created": x.created, "model": x.model, "choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}}}
	}
	data := func(value any) error { return emit("data: " + jsonText(value) + "\n\n") }
	if err := data(chunk(object{"role": "assistant"}, nil)); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	sent := ""
	for {
		visible, changed := x.visibleSnapshot()
		if strings.HasPrefix(visible, sent) && len(visible) > len(sent) {
			if err := data(chunk(object{"content": visible[len(sent):]}, nil)); err != nil {
				return err
			}
			sent = visible
		}
		select {
		case <-changed:
			continue
		case <-x.ready:
			if x.err != nil {
				return x.err
			}
			if len(x.message.ToolCalls) > 0 {
				calls := []any{}
				for i, call := range x.message.ToolCalls {
					calls = append(calls, object{"index": i, "id": call.ID, "type": "function", "function": call.Function})
				}
				if err := data(chunk(object{"tool_calls": calls}, nil)); err != nil {
					return err
				}
			} else if final := textValue(x.message.Content); strings.HasPrefix(final, sent) && len(final) > len(sent) {
				if err := data(chunk(object{"content": final[len(sent):]}, nil)); err != nil {
					return err
				}
			}
			// Usage rides on the finish chunk; stream_options.include_usage
			// asks for the separate empty-choices chunk instead.
			last := chunk(object{}, x.finish)
			if includeUsage {
				if err := data(last); err != nil {
					return err
				}
				last = object{"id": x.id, "object": "chat.completion.chunk", "created": x.created, "model": x.model, "choices": []any{}}
			}
			last["usage"] = apiUsage(x)
			return data(last)
		case <-ticker.C:
			if err := emit(": keepalive\n\n"); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
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
		writeAPIError(w, apiFail(400, "model_not_found", "Select a model listed by /v1/models"))
		return
	}
	x, failure := g.submit(req, r.Header.Get("Idempotency-Key"))
	if failure != nil {
		writeAPIError(w, failure)
		return
	}
	writeAPIExchange(w, r, x, req.Stream, truthy(req.StreamOptions["include_usage"]))
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
	g := newGateway(ctx, c, publicModels(models), dir, timeout)
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
