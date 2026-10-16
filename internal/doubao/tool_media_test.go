package doubao

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMediaObservationRecoveryNeverDuplicatesGeneration(t *testing.T) {
	for _, phase := range []string{"completed", "accepted", "unconfirmed"} {
		t.Run(phase, func(t *testing.T) {
			c, _ := newClient(t.TempDir())
			scope := toolMediaScope{model: Model{ID: "model"}, taskID: token(), nativeID: "native"}
			parts := []any{object{"type": "image_url", "image_url": object{"url": "data:image/png;base64,YQ=="}}}
			path := filepath.Join(c.dir, "gateway-media-"+scope.taskID+"-"+apiHash(object{"native_id": scope.nativeID, "content": parts})[:24]+".json")
			receipt := &Receipt{Dispatched: true, Accepted: phase != "unconfirmed", Completed: phase == "completed", ConversationID: "conv", RunID: "run", Reply: "observed value"}
			if err := saveJSON(path, receipt); err != nil {
				t.Fatal(err)
			}
			var generations, reads atomic.Int32
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/chat/completion" {
					generations.Add(1)
					return nil, context.Canceled
				}
				reads.Add(1)
				value := object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": "conv", "messages": []any{object{"message_id": "run", "user_type": 1}, object{"message_id": "reply", "user_type": 2, "bot_reply_message_id": "run", "status": 0, "ext": object{"is_finish": "1"}, "content_block": []any{object{"content": object{"text_block": object{"text": "observed value"}}}}}}}}}}}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jsonText(value))), Header: http.Header{}}, nil
			})
			text, err := c.observeToolMedia(context.Background(), scope, parts)
			if phase == "unconfirmed" {
				if err == nil {
					t.Fatal("uncertain media generation retried")
				}
			} else if err != nil || text != "observed value" {
				t.Fatal(text, err)
			}
			if generations.Load() != 0 || (phase != "accepted" && reads.Load() != 0) {
				t.Fatal("unexpected native operation")
			}
		})
	}
}
func TestCancellationIsTargetedAndNeverResent(t *testing.T) {
	c, _ := newClient(t.TempDir())
	path := filepath.Join(c.dir, "receipt.json")
	if err := saveJSON(path, &Receipt{Accepted: true, ConversationID: "conv", RunID: "original"}); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		result := object{}
		switch r.URL.Path {
		case "/passport/account/info/v2/":
			result = object{"data": object{"user_id_str": "account"}}
		case "/im/conversation/batch_get":
			result = object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": "conv", "messages": []any{object{"message_id": "original", "user_type": 1}}}}}}}
		case "/im/message/break_stream_msg":
			var body object
			if err := decode(r.Body, &body); err != nil {
				t.Fatal(err)
			}
			if field(body, "uplink_body", "break_stream_msg_uplink_body", "reply_msg_id") != "original" {
				t.Error("cancelled wrong run")
			}
			writes.Add(1)
			return nil, context.Canceled
		default:
			t.Errorf("unexpected cancellation route: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jsonText(result))), Header: http.Header{}}, nil
	})
	for i := 0; i < 2; i++ {
		if err := c.stopProbe(context.Background(), path, object{"action": "tool-stop"}); err == nil {
			t.Fatal("unconfirmed cancellation reported success")
		}
	}
	if writes.Load() != 1 {
		t.Fatal("cancellation duplicated", writes.Load())
	}
}
