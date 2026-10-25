package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// droppingBody delivers its events, then fails like a connection that broke.
type droppingBody struct{ r io.Reader }

func (b droppingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (droppingBody) Close() error { return nil }

const (
	resumeConversation = "12345678901234567"
	resumeRun          = "12345678901234568"
	resumeACK          = "event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"" + resumeConversation + "\"},\"query_list\":[{\"question_id\":\"" + resumeRun + "\"}]}\n\n"
	resumeFetch        = "event: FETCH_STREAM\ndata: {\"fetch_type\":2,\"fetch_key\":\"task-1\",\"append_scene\":1}\n\n"
)

func resumeEvent(name string, id int, data string) string {
	return "event: " + name + "\nid: " + resumeItoa(id) + "\ndata: " + data + "\n\n"
}

func resumeItoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// resumeUpstream fakes Work for one accepted run. completion is the body of
// the single generation; streams answers each task subscription in turn.
type resumeUpstream struct {
	mu          sync.Mutex
	sends       int
	readbacks   int
	seqStarts   []int
	completion  func() io.ReadCloser
	streams     []func() io.ReadCloser
	readbackErr bool
}

func (u *resumeUpstream) client(t *testing.T) *Client {
	t.Helper()
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sse := func(body io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
	}
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		u.mu.Lock()
		defer u.mu.Unlock()
		switch req.URL.Path {
		case "/chat/completion":
			u.sends++
			return sse(u.completion()), nil
		case "/chat/async/chunk_stream":
			var body object
			if err := decode(req.Body, &body); err != nil {
				t.Error(err)
			}
			u.seqStarts = append(u.seqStarts, number(body["seq_start"]))
			n := len(u.seqStarts) - 1
			if n >= len(u.streams) {
				n = len(u.streams) - 1
			}
			return sse(u.streams[n]()), nil
		case "/im/conversation/batch_get":
			u.readbacks++
			if u.readbackErr {
				return nil, errors.New("synthetic disconnect")
			}
			body := `{"status_code":0,"downlink_body":{"batch_get_conv_info_downlink_body":{"conversation_info_list":[{"conversation_id":"` + resumeConversation + `","messages":[{"user_type":1,"message_id":"` + resumeRun + `"},{"user_type":2,"message_id":"12345678901234569","bot_reply_message_id":"` + resumeRun + `","ext":{"is_finish":"1"},"content_block":[{"block_id":"answer","content":{"text_block":{"text":"RESUMED_OK"}}}]}]}]}}}`
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		return nil, errors.New("unexpected route " + req.URL.Path)
	})
	return c
}

func resumeBody(events string, drop bool) func() io.ReadCloser {
	return func() io.ReadCloser {
		if drop {
			return droppingBody{strings.NewReader(events)}
		}
		return io.NopCloser(strings.NewReader(events))
	}
}

func runResume(t *testing.T, c *Client, observe func(string, object) error) (*Receipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.runObserved(ctx, buildBody("fixture", "", "work", Model{ID: "1", Think: 1}), filepath.Join(c.dir, "receipt.json"), observe)
}

// A completion stream that breaks after Work accepted the run is followed by
// readback: the answer arrives and no second generation is submitted.
func TestDroppedCompletionStreamAfterACKFollowsByReadback(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK, true)}
	r, err := runResume(t, u.client(t), nil)
	if err != nil || !r.Completed || r.Reply != "RESUMED_OK" || u.sends != 1 || u.readbacks == 0 {
		t.Fatalf("err=%v completed=%v reply=%q sends=%d readbacks=%d", err, r.Completed, r.Reply, u.sends, u.readbacks)
	}
}

// Before ACK nothing proves Work accepted the run: the failure stands and
// nothing is resent or read back.
func TestDroppedCompletionStreamBeforeACKIsNotResumed(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody("", true)}
	r, err := runResume(t, u.client(t), nil)
	if !errors.Is(err, errSSERead) || r.Accepted || u.sends != 1 || u.readbacks != 0 {
		t.Fatalf("err=%v accepted=%v sends=%d readbacks=%d", err, r.Accepted, u.sends, u.readbacks)
	}
}

// A task stream that breaks is subscribed again from its cursor. Events up to
// the cursor are not observed twice.
func TestDroppedTaskStreamResumesFromCursor(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK+resumeFetch, false), streams: []func() io.ReadCloser{
		resumeBody(resumeEvent("CHUNK", 1, `{}`)+resumeEvent("CHUNK", 2, `{}`)+resumeEvent("CHUNK", 3, `{}`), true),
		resumeBody(resumeEvent("CHUNK", 3, `{}`)+resumeEvent("CHUNK", 4, `{}`)+resumeEvent("SSE_REPLY_END", 5, `{"end_type":3}`), false),
	}}
	chunks := 0
	r, err := runResume(t, u.client(t), func(name string, _ object) error {
		if name == "CHUNK" {
			chunks++
		}
		return nil
	})
	if err != nil || !r.Completed || u.sends != 1 || len(u.seqStarts) != 2 || u.seqStarts[0] != 0 || u.seqStarts[1] != 3 || chunks != 4 || r.Tasks[0].Seq != 5 {
		t.Fatalf("err=%v completed=%v sends=%d seqStarts=%v chunks=%d", err, r.Completed, u.sends, u.seqStarts, chunks)
	}
}

// A task stream that keeps breaking gets two more subscriptions; after that
// the run is followed by readback.
func TestTaskStreamThatKeepsDroppingFallsBackToReadback(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK+resumeFetch, false), streams: []func() io.ReadCloser{
		resumeBody(resumeEvent("CHUNK", 1, `{}`), true),
	}}
	r, err := runResume(t, u.client(t), nil)
	if err != nil || !r.Completed || r.Reply != "RESUMED_OK" || u.sends != 1 || len(u.seqStarts) != 3 {
		t.Fatalf("err=%v completed=%v sends=%d seqStarts=%v", err, r.Completed, u.sends, u.seqStarts)
	}
}

// A stream that ends cleanly without its terminal event is resubscribed too.
func TestUnterminatedTaskStreamIsResubscribed(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK+resumeFetch, false), streams: []func() io.ReadCloser{
		resumeBody(resumeEvent("CHUNK", 1, `{}`), false),
		resumeBody(resumeEvent("SSE_REPLY_END", 2, `{"end_type":3}`), false),
	}}
	r, err := runResume(t, u.client(t), nil)
	if err != nil || !r.Completed || len(u.seqStarts) != 2 || u.seqStarts[1] != 1 {
		t.Fatalf("err=%v completed=%v seqStarts=%v", err, r.Completed, u.seqStarts)
	}
}

// Fewer writes must not cost durability: ACK is on disk before the next
// event is handled, and the final receipt holds the answer.
func TestReceiptIsDurableAtACKAndCompletion(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK+resumeEvent("CHUNK", 1, `{}`), false)}
	c := u.client(t)
	path := filepath.Join(c.dir, "receipt.json")
	acked := false
	r, err := runResume(t, c, func(name string, _ object) error {
		if name != "CHUNK" {
			return nil
		}
		var saved Receipt
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &saved)
		}
		acked = err == nil && saved.Accepted && saved.RunID == resumeRun
		return nil
	})
	if err != nil || !acked {
		t.Fatal("ACK was not durable before the next event", err, acked)
	}
	var saved Receipt
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil || !saved.Completed || saved.Reply != r.Reply {
		t.Fatal("final receipt incomplete", err, saved.Completed)
	}
}
