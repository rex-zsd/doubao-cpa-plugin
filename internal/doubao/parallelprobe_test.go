package doubao

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentNativeDispatchDoesNotBlockSSEAndDeduplicates(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var uploaded atomic.Int32
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		uploaded.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	entered := make(chan string, 2)
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()
	d := &concurrentTools{c: c, dir: c.dir, sandbox: "owned", connector: "connector", seen: map[string]string{}, errors: make(chan error, 1),
		forward: func(ctx context.Context, call, input object) (object, error) {
			entered <- textValue(call["id"])
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return object{"id": call["id"], "name": call["name"], "tool_input": call["input"], "raw_result": `{"content":[],"isError":false}`}, nil
		}}
	frame := func(id, sandbox string) object {
		return object{"event_type": 1, "sandbox_id": sandbox, "tool_call": object{"id": id, "name": "connector.call", "input": `{}`}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, f := range []object{frame("ignored", "other"), frame("first", "owned"), frame("first", "owned"), frame("second", "owned")} {
		if err := d.handle(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-ctx.Done():
			t.Fatal("reader serialized behind unresolved first function")
		}
	}
	if uploaded.Load() != 0 {
		t.Fatal("a result was uploaded before release")
	}
	release()
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
	d.wg.Wait()
	if uploaded.Load() != 2 {
		t.Fatal("event replay duplicated a result", uploaded.Load())
	}
	select {
	case err := <-d.errors:
		t.Fatal(err)
	default:
	}
}

func TestConcurrentNativeDispatchRejectsReusedCallIdentity(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &concurrentTools{c: c, dir: c.dir, sandbox: "owned", seen: map[string]string{}, errors: make(chan error, 1), forward: func(ctx context.Context, call, input object) (object, error) { <-ctx.Done(); return nil, ctx.Err() }}
	defer func() { cancel(); d.wg.Wait() }()
	f := object{"event_type": 1, "sandbox_id": "owned", "tool_call": object{"id": "same", "name": "connector.call", "input": `{}`}}
	if err := d.handle(ctx, f); err != nil {
		t.Fatal(err)
	}
	changed := object{"event_type": 1, "sandbox_id": "owned", "tool_call": object{"id": "same", "name": "connector.call", "input": `{"different":true}`}}
	if err := d.handle(ctx, changed); err == nil {
		t.Fatal("reused ID with different arguments accepted")
	}
}
