package main

import (
	"context"
	"doubao-cpa-plugin/internal/doubao"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
)

// Native tasks outlive individual public tool-call responses. Host operations
// therefore belong to this plugin instance, not a transient callback scope.
// Cancellation and unload still release every host-owned stream/operation.
type hostTransport struct{}

func (hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, (32<<20)+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > 32<<20 {
			return nil, errors.New("upstream request exceeds 32 MiB")
		}
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	if err = hostCall("host.http.operation_open", map[string]any{}, &operation); err != nil {
		return nil, err
	}
	cancel := func() { _ = hostCall("host.http.cancel", map[string]any{"operation_id": operation.ID}, nil) }
	stop := context.AfterFunc(req.Context(), cancel)
	var result struct {
		Status  int         `json:"status_code"`
		Headers http.Header `json:"headers"`
		Stream  string      `json:"stream_id"`
	}
	err = hostCall("host.http.do_stream", map[string]any{"operation_id": operation.ID, "method": req.Method, "url": req.URL.String(), "headers": req.Header, "body": body, "wire_profile": map[string]any{"disable_redirects": true, "public_network_only": doubao.IsPublicMediaRequest(req.Context())}}, &result)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	// Upstream headers confirm that the request was written. Forward the trace
	// hook used to order tool-channel setup before submitting a generation.
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	r := &hostBody{id: result.Stream, cancel: cancel, stop: stop}
	return &http.Response{StatusCode: result.Status, Header: result.Headers, Body: r, Request: req, ContentLength: -1}, nil
}

type hostBody struct {
	id      string
	pending []byte
	ended   bool
	cancel  func()
	stop    func() bool
	once    sync.Once
}

func (b *hostBody) Read(out []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.ended {
			return 0, io.EOF
		}
		var frame struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := hostCall("host.http.stream_read", map[string]any{"stream_id": b.id}, &frame); err != nil {
			return 0, err
		}
		if frame.Error != "" {
			return 0, errors.New("upstream stream failed")
		}
		b.pending, b.ended = frame.Payload, frame.Done
	}
	n := copy(out, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *hostBody) Close() error {
	b.once.Do(func() {
		b.stop()
		_ = hostCall("host.http.stream_close", map[string]any{"stream_id": b.id}, nil)
		b.cancel()
	})
	return nil
}
