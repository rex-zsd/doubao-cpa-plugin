package doubao

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestTestStreamDropBreaksAfterDataEvents(t *testing.T) {
	stream := "\n\n\n: ping\n\nevent: A\r\ndata: 1\r\n\r\nevent: B\ndata: 2\n\ndata: 3\n\n"
	for _, chunked := range []bool{false, true} {
		var r io.Reader = strings.NewReader(stream)
		if chunked {
			r = iotest.OneByteReader(r)
		}
		got, err := io.ReadAll(&droppedBody{body: io.NopCloser(r), left: 2})
		if string(got) != "\n\n\n: ping\n\nevent: A\r\ndata: 1\r\n\r\nevent: B\ndata: 2\n\n" || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("chunked=%v got %q err %v", chunked, got, err)
		}
	}
}
