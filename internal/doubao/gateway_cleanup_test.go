package doubao

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real native runner, including stream shutdown and connector
// registration, while keeping cleanup stalled independently of generation.
type cleanupTransport struct {
	chat       fakeChat
	mu         sync.Mutex
	connectors map[string]bool
	n          int
	blocked    <-chan struct{}
	entered    chan string
	fail       bool
	failed     atomic.Bool
}

func (f *cleanupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	status := object{"code": 0}
	switch r.URL.Path {
	case "/alice/office/skills/manage/connector/create_personal":
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("connector-%d", f.n)
		if f.connectors == nil {
			f.connectors = map[string]bool{}
		}
		f.connectors[id] = true
		f.mu.Unlock()
		status["data"] = object{"connector": object{"connector_id": id}}
	case "/alice/office/skills/manage/connector/list_by_user":
		if f.failed.Load() {
			return nil, fmt.Errorf("fixture cleanup readback failed")
		}
		items := []any{}
		f.mu.Lock()
		for id, present := range f.connectors {
			if present {
				items = append(items, object{"connector_id": id, "enabled": true})
			}
		}
		f.mu.Unlock()
		status["data"] = object{"items": items, "has_more": false}
	case "/alice/office/skills/manage/connector/disconnect":
		var body object
		if err := decode(r.Body, &body); err != nil {
			return nil, err
		}
		id := textValue(body["connector_id"])
		f.entered <- id
		select {
		case <-f.blocked:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		if f.fail {
			f.failed.Store(true)
			return nil, fmt.Errorf("fixture disconnect failed")
		}
		f.mu.Lock()
		f.connectors[id] = false
		f.mu.Unlock()
	case "/alice/office/tool_local/chunk_stream":
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: untilCancelled{r.Context()}}, nil
	default:
		return f.chat.RoundTrip(r)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jsonText(status)))}, nil
}

func awaitCleanup(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start")
		return ""
	}
}

func readCleanupState(t *testing.T, path string) object {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state object
	if err = decode(strings.NewReader(string(b)), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNativeAnswerReturnsBeforeCleanupAndContinues(t *testing.T) {
	g, server := testGateway(t)
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	g.ctx = ctx
	g.models[0].Efforts = []int{3, 4, 5, 6, 7}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f := &cleanupTransport{blocked: release, entered: make(chan string, 2)}
	g.c.http.Transport = f
	req := toolRequest()
	req.Effort = 7
	req.Tools = append(req.Tools, apiTool{Type: "function", Function: apiFunction{Name: "second_tool", Description: "Preserve this exact tool description.", Parameters: object{"type": "object"}}})
	status, response := postGateway(t, server, req, "cleanup-replay")
	if status != 200 {
		t.Fatal(status, response)
	}
	first := returnedMessage(t, response)
	if first.Content != "reply 2000000000001" {
		t.Fatal("answer changed", first)
	}
	awaitCleanup(t, f.entered)
	g.mu.Lock()
	turn := g.idempotency[apiHash("cleanup-replay")].turn
	statePath, folder, sandbox := turn.statePath, turn.workspace, turn.sandbox
	g.mu.Unlock()
	state := readCleanupState(t, statePath)
	if state["done"] != true || state["cleanup_requested"] != true || state["cleanup_confirmed"] == true || state["final_text"] != first.Content {
		t.Fatal("final answer and pending cleanup were not durable", state["done"], state["cleanup_requested"])
	}
	_, replay := postGateway(t, server, req, "cleanup-replay")
	if jsonText(replay) != jsonText(response) {
		t.Fatal("retry regenerated or changed answer")
	}
	// The completed stream/handler must release its sandbox before finish.
	g.inboxMu.Lock()
	_, oldStream := g.inboxes[sandbox]
	g.inboxMu.Unlock()
	if oldStream {
		t.Fatal("answer returned while old native handler still owned sandbox")
	}
	next := req
	next.Messages = append(append([]apiMessage{}, req.Messages...), first, apiMessage{Role: "user", Content: "Continue exactly with both tools and maximum effort."})
	status, response = postGateway(t, server, next, "cleanup-next")
	if status != 200 || returnedMessage(t, response).Content != "reply 2000000000002" {
		t.Fatal("continued answer failed", status, response)
	}
	awaitCleanup(t, f.entered)
	g.mu.Lock()
	continued := g.idempotency[apiHash("cleanup-next")].turn
	continuedOK := continued.conversation == turn.conversation && continued.workspace == folder && continued.sandbox == sandbox && continued.continued == 2
	// Pending cleanup must prevent even expired turns from being evicted.
	turn.finished = time.Now().Add(-24 * time.Hour)
	g.sweepLocked()
	retained := g.turns[turn.id] == turn
	g.mu.Unlock()
	if !continuedOK || !retained {
		t.Fatal("pending cleanup broke continuation or retention", continuedOK, retained)
	}
	if _, err := os.Stat(folder); err != nil {
		t.Fatal("workspace removed before cleanup", err)
	}
	// The exact request still owns both tools and effort 7; verify the sent
	// prompt and schema, not just the returned fixture answer.
	part := next
	part.Messages = next.Messages[2:]
	if sentText(f.chat.last()) != renderAPIPrompt(part, connectorName(continued.id), true) {
		t.Fatal("continued model prompt changed")
	}
	schema, err := os.ReadFile(filepath.Join(g.dir, "gateway-schema-"+continued.id+".json"))
	if err != nil || !strings.Contains(string(schema), "Preserve this exact tool description.") || !strings.Contains(string(schema), "client_nonce") {
		t.Fatal("tool catalog changed", err)
	}
	if continued.request.Effort != 7 || field(f.chat.last(), "ext", "reasoning_effort") != "7" || jsonText(continued.request.Tools) != jsonText(req.Tools) {
		t.Fatal("request capability changed")
	}
	// The worker remains tracked even though its answer has been delivered.
	joined, closing := make(chan struct{}), make(chan struct{})
	service := &Service{g: g, c: g.c, cancel: cancel}
	go func() { close(closing); service.Close(); close(joined) }()
	<-closing
	select {
	case <-joined:
		t.Fatal("cleanup worker escaped gateway lifetime")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup worker leaked")
	}
	state = readCleanupState(t, statePath)
	if state["cleanup_confirmed"] != true {
		t.Fatal("cleanup did not persist confirmation")
	}
	f.chat.mu.Lock()
	defer f.chat.mu.Unlock()
	if len(f.chat.bodies) != 2 {
		t.Fatal("duplicate generation", len(f.chat.bodies))
	}
}

func TestCleanupFailureKeepsVerifiedAnswerAndPendingState(t *testing.T) {
	g, server := testGateway(t)
	release := make(chan struct{})
	close(release)
	f := &cleanupTransport{blocked: release, entered: make(chan string, 1), fail: true}
	g.c.http.Transport = f
	req := toolRequest()
	status, answer := postGateway(t, server, req, "cleanup-failure")
	if status != 200 || returnedMessage(t, answer).Content != "reply 2000000000001" {
		t.Fatal("cleanup changed success", status, answer)
	}
	g.wg.Wait()
	_, again := postGateway(t, server, req, "cleanup-failure")
	if jsonText(again) != jsonText(answer) {
		t.Fatal("cleanup failure changed cached response")
	}
	g.mu.Lock()
	turn := g.idempotency[apiHash("cleanup-failure")].turn
	statePath := turn.statePath
	g.mu.Unlock()
	state := readCleanupState(t, statePath)
	if state["done"] != true || state["cleanup_confirmed"] == true || state["cleanup_requested"] != true || state["error"] != nil {
		t.Fatal("cleanup failure lost pending state or answer")
	}
}

func TestCleanupLeavesUnfinishedSuspendedTaskIntact(t *testing.T) {
	g, _ := testGateway(t)
	turn := persistedTurn(t, g, false)
	defer turn.cancel()
	turn.connector = "resume-this-connector"
	g.suspending = true
	var calls atomic.Int32
	g.c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.Canceled })
	g.cleanupFinished(turn)
	if calls.Load() != 0 || turn.cleanupRequested || turn.done {
		t.Fatal("suspended task was cleaned up")
	}
}

func TestFinishedCleanupRecoveryDoesNotResendOrChangeAnswer(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(fmt.Sprint("requested=", requested), func(t *testing.T) {
			g, _ := testGateway(t)
			turn := persistedTurn(t, g, true)
			defer turn.cancel()
			turn.connector, turn.cleanupRequested = "old-connector", requested
			if err := g.checkpointLocked(turn); err != nil {
				t.Fatal(err)
			}
			release, entered := make(chan struct{}), make(chan string, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var disconnects, generations atomic.Int32
			g.c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/alice/office/skills/manage/connector/disconnect":
					disconnects.Add(1)
				case "/alice/office/skills/manage/connector/list_by_user":
					entered <- "readback"
					<-release
				case "/chat/completion":
					generations.Add(1)
				default:
					return nil, fmt.Errorf("unexpected recovery request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"items":[],"has_more":false}}`))}, nil
			})
			restored := newGateway(g.ctx, g.c, g.models, g.dir, time.Minute)
			if err := restored.recoverTasks(); err != nil {
				t.Fatal(err)
			}
			awaitCleanup(t, entered)
			x, failure := restored.submit(turn.request, "restart")
			if failure != nil || x.id != turn.first.id || x.message.Content != "cached answer" {
				t.Fatal("pending cleanup blocked/changed replay", failure)
			}
			if generations.Load() != 0 || disconnects.Load() != int32(map[bool]int{false: 1, true: 0}[requested]) {
				t.Fatal("recovery repeated an uncertain mutation")
			}
			unblock()
			restored.wg.Wait()
			if readCleanupState(t, turn.statePath)["cleanup_confirmed"] != true {
				t.Fatal("recovered cleanup not confirmed")
			}
		})
	}
}

// The stream may acknowledge cancellation before its handler/reader exits.
// A resumed answer must wait for that exit, then skip waiting for cleanup.
type delayedNativeStream struct {
	ctx      context.Context
	started  chan struct{}
	stopping chan struct{}
	release  <-chan struct{}
}

func (r *delayedNativeStream) Read([]byte) (int, error) {
	close(r.started)
	<-r.ctx.Done()
	close(r.stopping)
	<-r.release
	return 0, r.ctx.Err()
}
func (r *delayedNativeStream) Close() error { return nil }

func TestResumedAnswerJoinsNativeStreamBeforeCleanup(t *testing.T) {
	g, _ := testGateway(t)
	turn := persistedTurn(t, g, false)
	defer turn.cancel()
	turn.connector, turn.sandbox, turn.mode = "connector", "recovered-sandbox", "tools"
	if err := saveJSON(turn.receiptPath, &Receipt{Accepted: true, ConversationID: fakeConversation(1), RunID: "2000000000001"}); err != nil {
		t.Fatal(err)
	}
	if err := g.checkpointLocked(turn); err != nil {
		t.Fatal(err)
	}
	streamStarted, streamStopping, releaseStream := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseCleanup, cleanupEntered := make(chan struct{}), make(chan string, 1)
	var streamOnce, cleanupOnce sync.Once
	unblockStream := func() { streamOnce.Do(func() { close(releaseStream) }) }
	unblockCleanup := func() { cleanupOnce.Do(func() { close(releaseCleanup) }) }
	defer unblockStream()
	defer unblockCleanup()
	chat := &fakeChat{runs: map[string]string{fakeConversation(1): "2000000000001"}}
	g.c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/alice/office/tool_local/chunk_stream":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &delayedNativeStream{r.Context(), streamStarted, streamStopping, releaseStream}}, nil
		case "/im/conversation/batch_get":
			<-streamStarted
			return chat.RoundTrip(r)
		case "/alice/office/skills/manage/connector/disconnect":
			cleanupEntered <- "disconnect"
			<-releaseCleanup
		case "/alice/office/skills/manage/connector/list_by_user":
		default:
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"items":[],"has_more":false}}`))}, nil
	})
	restored := newGateway(g.ctx, g.c, g.models, g.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	x, failure := restored.submit(turn.request, "restart")
	if failure != nil {
		t.Fatal(failure)
	}
	select {
	case <-streamStopping:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not cancelled")
	}
	select {
	case <-x.ready:
		t.Fatal("answer exposed sandbox before stream joined")
	default:
	}
	unblockStream()
	select {
	case <-x.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("answer stalled on cleanup")
	}
	if x.err != nil || x.message.Content != "reply 2000000000001" {
		t.Fatal("recovered answer changed", x.err)
	}
	awaitCleanup(t, cleanupEntered)
	unblockCleanup()
	restored.wg.Wait()
}
