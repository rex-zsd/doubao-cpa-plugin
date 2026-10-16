package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVisibleStreamFiltersReasoningAndUnknownParents(t *testing.T) {
	s := &nativeVisibleStream{}
	s.observe("SSE_ACK", object{"query_list": []any{object{"question_id": "run"}}})
	s.observe("STREAM_MSG_NOTIFY", object{"meta": object{"message_id": "message", "bot_reply_message_id": "run", "user_type": 2}, "content": object{"content_block": []any{object{"block_id": "clock", "block_type": 10091}}}})
	patch := func(block object) string {
		return s.observe("STREAM_CHUNK", object{"message_id": "message", "patch_op": []any{object{"patch_value": object{"content_block": []any{block}}}}})
	}
	for _, block := range []object{
		{"block_id": "child", "block_type": 10000, "parent_id": "unseen-parent", "patch_type": 1, "content": object{"text_block": object{"text": "HIDDEN_CHILD"}}},
		{"block_id": "reason", "block_type": 10040, "content": object{"thinking_block": object{"content": "HIDDEN_REASONING"}}},
		{"block_id": "unknown", "block_type": 10000, "control_info": object{"collapse_block_id": "unseen"}, "content": object{"text_block": object{"text": "HIDDEN_UNKNOWN"}}},
	} {
		if text := patch(block); text != "" {
			t.Fatal("hidden content leaked", text)
		}
	}
	if text := patch(object{"block_id": "answer", "block_type": 10000, "patch_type": 1, "control_info": object{"collapse_block_id": "clock"}, "content": object{"text_block": object{"text": "Hello"}}}); text != "Hello" {
		t.Fatal(text)
	}
	if text := patch(object{"block_id": "answer", "block_type": 10000, "patch_type": 1, "content": object{"text_block": object{"text": " world"}}}); text != "Hello world" {
		t.Fatal(text)
	}
	if text := patch(object{"block_id": "answer", "block_type": 10000, "patch_type": 2, "content": object{"text_block": object{"text": "Hello world!"}}}); text != "Hello world!" {
		t.Fatal(text)
	}
}
func TestStreamingPublishesBeforeCompletionAndReplaysExactly(t *testing.T) {
	x := newExchange("TestModel")
	first := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	var output strings.Builder
	go func() {
		finished <- (&Response{x: x}).Stream(context.Background(), func(data []byte) error {
			output.Write(data)
			if strings.Contains(string(data), `"content":"Hello"`) {
				select {
				case <-first:
				default:
					close(first)
				}
			}
			return nil
		})
	}()
	x.updateVisible("Hello")
	<-first
	go func() { <-release; x.resolve(apiMessage{Role: "assistant", Content: "Hello world"}, "stop", nil) }()
	select {
	case <-x.ready:
		t.Fatal("completed before incremental text read")
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"content":" world"`) || strings.Count(output.String(), `"content":"Hello"`) != 1 {
		t.Fatal("bad incremental stream", output.String())
	}
	var replay strings.Builder
	if err := (&Response{x: x}).Stream(context.Background(), func(data []byte) error { replay.Write(data); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Count(replay.String(), `"content":"Hello"`) != 1 || !strings.Contains(replay.String(), `"content":" world"`) {
		t.Fatal("replay duplicated/lost text")
	}
	y := newExchange("TestModel")
	y.updateVisible("old prefix")
	y.resolve(apiMessage{Role: "assistant", Content: "revised answer"}, "stop", nil)
	if y.err == nil || y.err.Code != "visible_text_changed" {
		t.Fatal("revised upstream output silently accepted")
	}
}

func TestStreamReturnsFailureAndHonorsIncludeUsage(t *testing.T) {
	x := newExchange("TestModel")
	x.resolve(apiMessage{}, "", promptTooLong("word"))
	if err := (&Response{x: x}).Stream(context.Background(), func([]byte) error { return nil }); err != x.err {
		t.Fatal("stream hid the upstream failure", err)
	}
	y := newExchange("TestModel")
	y.resolve(apiMessage{Role: "assistant", Content: "hi"}, "stop", nil)
	for _, usage := range []bool{false, true} {
		var frames []object
		err := (&Response{x: y, usage: usage}).Stream(context.Background(), func(data []byte) error {
			var frame object
			if json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(data), []byte("data: ")), &frame) == nil {
				frames = append(frames, frame)
			}
			return nil
		})
		last := frames[len(frames)-1]
		finish := field(array(frames[len(frames)-2]["choices"])[0], "finish_reason")
		if err != nil || last["usage"] == nil || usage != (len(array(last["choices"])) == 0) || usage && (finish != "stop" || frames[len(frames)-2]["usage"] != nil) {
			t.Fatal("usage chunk does not follow stream_options.include_usage", usage, frames)
		}
	}
}
