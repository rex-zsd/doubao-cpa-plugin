package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const lookupConv = "12345678901230001"
const lookupRun = "12345678901230002"
const lookupReply = "12345678901230003"

func lookupMessage() object {
	return object{"conversation_id": lookupConv, "message_id": lookupReply, "bot_reply_message_id": lookupRun, "user_type": 2, "status": 0, "content_status": 0, "ext": object{"is_finish": "1", "context_window_usage": `{"system_prompt":7,"messages":11,"tools":2,"skills":3}`}, "content_block": []any{object{"block_type": 10000, "content": object{"text_block": object{"text": "verified answer"}}}}}
}

func lookupFrame(name string, message object) string {
	data := object{"cmd": 2270, "status_code": 0, "downlink_body": object{"load_message_stream_downlink_body": object{"message_id": lookupReply, "message_body": message}}}
	return "event: " + name + "\ndata: " + jsonText(data) + "\n\n"
}

func lookupReceipt() *Receipt {
	r := &Receipt{Accepted: true, StreamEnded: true, ConversationID: lookupConv, RunID: lookupRun, LocalID: "local", Events: map[string]int{}, Request: buildBody("fixture", "", "work", Model{ID: "1"})}
	r.observeInlineReply("FULL_MSG_NOTIFY", object{"message": object{"conversation_id": lookupConv, "message_id": lookupRun, "local_message_id": "local", "user_type": 1}})
	r.observeInlineReply("STREAM_MSG_NOTIFY", object{"meta": object{"conversation_id": lookupConv, "message_id": lookupReply, "bot_reply_message_id": lookupRun, "user_type": 2}})
	r.observeInlineReply("SSE_REPLY_END", object{"end_type": 1, "msg_finish_attr": object{"msgid": lookupReply}})
	return r
}

func conversationLookupResponse() *http.Response {
	root := object{"user_type": 1, "message_id": lookupRun}
	body := object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": lookupConv, "messages": []any{root, lookupMessage()}}}}}}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(jsonText(body)))}
}

func TestLiveInlineReplyReadsOneFinalMessageWithoutRegeneration(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sends, lookups := 0, 0
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body object
		if err := decode(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		switch req.URL.Path {
		case "/chat/completion":
			sends++
			local := field(array(body["messages"])[0], "local_message_id")
			var stream strings.Builder
			for _, event := range []struct {
				name string
				data object
			}{
				{"SSE_ACK", object{"ack_client_meta": object{"conversation_id": lookupConv}, "query_list": []any{object{"question_id": lookupRun, "local_message_id": local}}}},
				{"FULL_MSG_NOTIFY", object{"message": object{"user_type": 1, "conversation_id": lookupConv, "message_id": lookupRun, "local_message_id": local}}},
				{"STREAM_MSG_NOTIFY", object{"meta": object{"user_type": 2, "conversation_id": lookupConv, "message_id": lookupReply, "bot_reply_message_id": lookupRun}}},
				{"SSE_REPLY_END", object{"end_type": 1, "msg_finish_attr": object{"msgid": lookupReply}}},
				{"SSE_REPLY_END", object{"end_type": 3}},
			} {
				stream.WriteString("event: " + event.name + "\ndata: " + jsonText(event.data) + "\n\n")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
		case "/im/message/stream_load":
			lookups++
			if number(body["cmd"]) != 2270 || field(body, "uplink_body", "load_message_stream_uplink_body", "conversation_id") != lookupConv || field(body, "uplink_body", "load_message_stream_uplink_body", "message_id") != lookupReply {
				t.Fatal("wrong native lookup request", body)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(lookupFrame("FULL_MSG", lookupMessage())))}, nil
		default:
			t.Fatal("unexpected endpoint", req.URL.Path)
			return nil, errors.New("unexpected endpoint")
		}
	})
	r, err := c.run(context.Background(), buildBody("fixture", "", "work", Model{ID: "1"}), filepath.Join(c.dir, "receipt.json"))
	if err != nil || !r.Completed || r.Reply != "verified answer" || r.Context != 23 || r.Readback != "message" || sends != 1 || lookups != 1 {
		t.Fatal("native final lookup failed", r, err, sends, lookups)
	}
}

func TestInlineLookupFallsBackWhenProofOrSnapshotIsIncomplete(t *testing.T) {
	for _, name := range []string{"no-root", "no-finish", "wrong-finish", "second-finish", "missing-meta", "foreign-meta", "unknown-full-meta", "repeated-ack", "no-local", "ambiguous", "async", "async-with-task", "local", "recovered", "not-ended", "wrong-id", "wrong-parent", "wrong-role", "missing-status", "unfinished", "missing-usage", "complex-task", "transport", "truncated", "not-sse", "refused", "split-status", "split-parent", "split-ext"} {
		t.Run(name, func(t *testing.T) {
			c, _ := newClient(t.TempDir())
			r := lookupReceipt()
			message := lookupMessage()
			wantLookups := 1
			switch name {
			case "no-root":
				r.inlineReadback.rootVerified = false
				wantLookups = 0
			case "no-finish":
				r.inlineReadback.finished = false
				wantLookups = 0
			case "wrong-finish":
				r.inlineReadback.finished = false
				r.observeInlineReply("SSE_REPLY_END", object{"end_type": 1, "msg_finish_attr": object{"msgid": "other"}})
				wantLookups = 0
			case "ambiguous":
				r.observeInlineReply("STREAM_MSG_NOTIFY", object{"meta": object{"user_type": 2, "conversation_id": lookupConv, "message_id": "other", "bot_reply_message_id": lookupRun}})
				wantLookups = 0
			case "async":
				r.Events["FETCH_STREAM"] = 1
				wantLookups = 0
			case "second-finish":
				r.observeInlineReply("SSE_REPLY_END", object{"end_type": 1, "msg_finish_attr": object{"msgid": "other"}})
				wantLookups = 0
			case "missing-meta":
				r.observeInlineReply("STREAM_MSG_NOTIFY", object{})
				wantLookups = 0
			case "foreign-meta":
				r.observeInlineReply("STREAM_MSG_NOTIFY", object{"meta": object{"user_type": 2, "conversation_id": "other", "message_id": lookupReply, "bot_reply_message_id": lookupRun}})
				wantLookups = 0
			case "unknown-full-meta":
				r.observeInlineReply("FULL_MSG_NOTIFY", object{})
				wantLookups = 0
			case "repeated-ack":
				r.observeInlineReply("SSE_ACK", object{})
				wantLookups = 0
			case "no-local":
				r.LocalID = ""
				wantLookups = 0
			case "async-with-task":
				r.Tasks = []*task{{ID: "pending"}}
				wantLookups = 0
			case "local":
				field(r.Request, "option", "general_task_param").(map[string]any)["runtime_type"] = 2
				wantLookups = 0
			case "recovered":
				r.inlineReadback = nil
				wantLookups = 0
			case "not-ended":
				r.StreamEnded = false
				wantLookups = 0
			case "wrong-id":
				message["message_id"] = "other"
			case "wrong-parent":
				message["bot_reply_message_id"] = "other"
			case "wrong-role":
				message["user_type"] = 1
			case "missing-status":
				delete(message, "status")
			case "unfinished":
				field(message, "ext").(map[string]any)["is_finish"] = "0"
			case "missing-usage":
				delete(field(message, "ext").(map[string]any), "context_window_usage")
			case "complex-task":
				message["content_block"] = []any{object{"content": object{"complex_task_block": object{}}}}
			}
			lookups, fallback := 0, 0
			c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/im/conversation/batch_get" {
					fallback++
					return conversationLookupResponse(), nil
				}
				if req.URL.Path != "/im/message/stream_load" {
					t.Fatal(req.URL.Path)
				}
				lookups++
				if name == "transport" {
					return nil, errors.New("synthetic transport failure")
				}
				raw, contentType := lookupFrame("FULL_MSG", message), "text/event-stream"
				if name == "truncated" {
					raw = strings.TrimSuffix(raw, "\n\n")
				}
				if name == "not-sse" {
					contentType = "application/json"
					raw = "{}"
				}
				if name == "refused" {
					raw = "event: STREAM_MSG_ERROR\ndata: {}\n\n"
				}
				if strings.HasPrefix(name, "split-") {
					part := object{}
					switch name {
					case "split-status":
						part["status"] = 11
					case "split-parent":
						part["bot_reply_message_id"] = "other"
					case "split-ext":
						part["ext"] = object{"is_interrupted": "true"}
					}
					raw = lookupFrame("STREAM_MSG_START", message) + lookupFrame("STREAM_MSG_END", part)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})
			answer, finished, err := c.readReply(context.Background(), r)
			if err != nil || !finished || answer != "verified answer" || r.Context != 23 || r.Readback != "conversation" || lookups != wantLookups || fallback != 1 || r.inlineReadback != nil {
				t.Fatal("lookup did not fall back safely", answer, finished, err, lookups, fallback)
			}
		})
	}
}

func TestInlineLookupRetainsBlockedFailureCancellationAndInputStates(t *testing.T) {
	for _, name := range []string{"blocked", "blocked-v2", "failed", "cancelled", "waiting"} {
		t.Run(name, func(t *testing.T) {
			c, _ := newClient(t.TempDir())
			r := lookupReceipt()
			message := lookupMessage()
			switch name {
			case "blocked":
				message["status"] = 11
			case "blocked-v2":
				message["status_v2"] = 11
			case "failed":
				message["content_status"] = 500
			case "cancelled":
				message["content_status"] = 120
			case "waiting":
				message["content_block"] = []any{object{"content": object{"interaction_ask_block": object{"status": 1}}}}
			}
			c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/im/message/stream_load" {
					t.Fatal("terminal failure incorrectly fell back", req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(lookupFrame("FULL_MSG", message)))}, nil
			})
			answer, finished, err := c.readReply(context.Background(), r)
			if err == nil || finished || answer != "" || r.Context != 0 {
				t.Fatal("terminal state returned as success", answer, finished, err)
			}
		})
	}
}

func TestNativeMessageLoaderJoinsSplitBodyWithoutLosingMetadata(t *testing.T) {
	c, _ := newClient(t.TempDir())
	message := lookupMessage()
	message["content_block"] = []any{object{"block_type": 10000, "content": object{"text_block": object{"text": "first"}}}}
	raw := lookupFrame("STREAM_MSG_START", message) + lookupFrame("STREAM_MSG_CHUNK", object{"content_block": []any{object{"block_type": 10000, "content": object{"text_block": object{"text": "second"}}}}}) + lookupFrame("STREAM_MSG_END", object{})
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})
	loaded, err := c.loadMessage(context.Background(), lookupConv, lookupReply)
	if err != nil || visibleText(loaded) != "first\nsecond" || contextTokens(loaded) != 23 || field(loaded, "bot_reply_message_id") != lookupRun {
		t.Fatal("split message lost content or metadata", loaded, err)
	}
}

func TestConversationReadbackRejectsWithheldV2Status(t *testing.T) {
	c, _ := newClient(t.TempDir())
	r := lookupReceipt()
	r.inlineReadback = nil
	message := lookupMessage()
	message["status_v2"] = 11
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/im/conversation/batch_get" {
			t.Fatal(req.URL.Path)
		}
		body := object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": lookupConv, "messages": []any{object{"user_type": 1, "message_id": lookupRun}, message}}}}}}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jsonText(body)))}, nil
	})
	answer, finished, err := c.readReply(context.Background(), r)
	if !errors.Is(err, errContentBlocked) || finished || answer != "" || r.Context != 0 {
		t.Fatal("withheld final message returned as success", answer, finished, err)
	}
}
