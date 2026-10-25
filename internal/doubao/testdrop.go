package doubao

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// testStreamDrop is a live-test hook of the helper's serve action, set by
// DOUBAO_HELPER_TEST_DROP=<path>:<events>: the first response on that path
// breaks after the given number of SSE events, like a dropped connection.
// The plugin never installs it.
type testStreamDrop struct {
	next   http.RoundTripper
	path   string
	events int
	fired  atomic.Bool
}

func installTestStreamDrop(c *Client, spec string) bool {
	path, count, ok := strings.Cut(spec, ":")
	events, err := strconv.Atoi(count)
	if !ok || err != nil || events < 0 || !strings.HasPrefix(path, "/") {
		return false
	}
	c.http.Transport = &testStreamDrop{next: c.http.Transport, path: path, events: events}
	return true
}

func (d *testStreamDrop) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := d.next.RoundTrip(req)
	if err != nil || req.URL.Path != d.path || !d.fired.CompareAndSwap(false, true) {
		return response, err
	}
	response.Body = &droppedBody{body: response.Body, left: d.events}
	return response, nil
}

// droppedBody counts SSE events with data, ignoring padding, comments and
// blank lines, and fails the read after the last allowed one.
type droppedBody struct {
	body  io.ReadCloser
	left  int
	line  []byte
	event bool
	cut   bool
}

func (b *droppedBody) Read(p []byte) (int, error) {
	if b.cut || b.left == 0 {
		b.cut = true
		return 0, io.ErrUnexpectedEOF
	}
	n, err := b.body.Read(p)
	for i := 0; i < n; i++ {
		switch p[i] {
		case '\r':
		case '\n':
			if len(b.line) == 0 && b.event {
				b.event = false
				if b.left--; b.left == 0 {
					b.cut = true
					return i + 1, nil
				}
			}
			if strings.HasPrefix(string(b.line), "data:") {
				b.event = true
			}
			b.line = b.line[:0]
		default:
			if len(b.line) < 5 {
				b.line = append(b.line, p[i])
			} else if len(b.line) == 5 {
				b.line = append(b.line, '.') // a longer line; its prefix is kept
			}
		}
	}
	return n, err
}
func (b *droppedBody) Close() error { return b.body.Close() }
