package doubao

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
)

// Only an uninterrupted cloud Work stream with a single verified root reply
// can use per-message retrieval. Async, local-tool, ambiguous and recovered
// runs retain conversation readback. This changes the final lookup, not the
// criteria for declaring the generation complete.
type inlineReplyProof struct {
	rootVerified, finished, ambiguous bool
	messageID                         string
}

func (r *Receipt) observeInlineReply(name string, data object) {
	if name == "SSE_ACK" {
		// A repeated acceptance event needs fresh identity evidence too.
		r.inlineReadback = nil
		return
	}
	if !r.Accepted {
		return
	}
	if r.inlineReadback == nil {
		r.inlineReadback = &inlineReplyProof{}
	}
	p := r.inlineReadback
	var meta any
	switch name {
	case "FULL_MSG_NOTIFY":
		meta = data["message"]
		if number(field(meta, "user_type")) == 1 {
			if r.LocalID != "" && textValue(field(meta, "conversation_id")) == r.ConversationID && textValue(field(meta, "message_id")) == r.RunID && textValue(field(meta, "local_message_id")) == r.LocalID {
				p.rootVerified = true
			} else {
				p.ambiguous = true
			}
			return
		}
	case "STREAM_MSG_NOTIFY":
		meta = data["meta"]
	case "SSE_REPLY_END":
		if number(data["end_type"]) == 1 {
			if p.messageID != "" && textValue(field(data, "msg_finish_attr", "msgid")) == p.messageID {
				p.finished = true
			} else {
				p.ambiguous = true
			}
		}
		return
	default:
		return
	}
	if number(field(meta, "user_type")) != 2 || textValue(field(meta, "conversation_id")) != r.ConversationID {
		p.ambiguous = true
		return
	}
	if textValue(field(meta, "bot_reply_message_id")) != r.RunID {
		p.ambiguous = true
		return
	}
	id := textValue(field(meta, "message_id"))
	if id == "" {
		p.ambiguous = true
		return
	}
	if p.messageID != "" && p.messageID != id {
		p.ambiguous = true
	}
	p.messageID = id
}

func (c *Client) readInlineReply(ctx context.Context, r *Receipt) (string, bool, bool, error) {
	p := r.inlineReadback
	// One attempt at most; any uncertainty falls back to the existing lookup.
	r.inlineReadback = nil
	if p == nil || !r.Accepted || !r.StreamEnded || r.LocalID == "" || !p.rootVerified || !p.finished || p.ambiguous || p.messageID == "" || len(r.Tasks) != 0 || r.Events["FETCH_STREAM"] != 0 || number(field(r.Request, "option", "agent_mode")) != 1 || number(field(r.Request, "option", "general_task_param", "runtime_type")) != 1 {
		return "", false, false, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	message, err := c.loadMessage(lookupCtx, r.ConversationID, p.messageID)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, true, ctx.Err()
		}
		return "", false, false, nil
	}
	if textValue(field(message, "conversation_id")) != r.ConversationID || textValue(field(message, "message_id")) != p.messageID || number(field(message, "user_type")) != 2 || textValue(field(message, "bot_reply_message_id")) != r.RunID {
		return "", false, false, nil
	}
	if number(field(message, "status")) == 11 || number(field(message, "status_v2")) == 11 {
		return "", false, true, errContentBlocked
	}
	if field(message, "status") == nil || number(field(message, "status")) != 0 {
		return "", false, false, nil
	}
	answer, finished, err := finishedReply(message, r)
	if !finished && err == nil {
		return "", false, false, nil
	}
	if finished && contextTokens(message) == 0 {
		// A narrower response must not lose the actual Work context usage.
		return "", false, false, nil
	}
	if finished {
		r.Readback = "message"
	}
	return answer, finished, true, err
}

// Source: official web full-message loader, LOAD_MESSAGE_STREAM (2270).
// It returns a complete message with status, root identity and context usage.
func (c *Client) loadMessage(ctx context.Context, conversation, messageID string) (object, error) {
	body := object{"cmd": 2270, "uplink_body": object{"load_message_stream_uplink_body": object{"conversation_id": conversation, "message_id": messageID, "ext": object{}}}, "sequence_id": token(), "channel": 2, "version": "1"}
	response, err := c.request(ctx, "POST", "/im/message/stream_load", nil, body)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		return nil, errors.New("message lookup is not SSE")
	}
	var message object
	terminal, frames := false, 0
	err = consumeSSE(io.LimitReader(response.Body, 8<<20), func(name string, data object, _ string) (bool, error) {
		frames++
		if frames > 256 {
			return false, errors.New("message lookup frame limit")
		}
		if name == "gateway-error" || name == "STREAM_MSG_ERROR" {
			return false, errors.New("message lookup refused")
		}
		if name != "FULL_MSG" && name != "STREAM_MSG_START" && name != "STREAM_MSG_CHUNK" && name != "STREAM_MSG_END" {
			return false, nil
		}
		if field(data, "status_code") == nil || number(data["status_code"]) != 0 {
			return false, errors.New("message lookup rejected")
		}
		part, ok := field(data, "downlink_body", "load_message_stream_downlink_body", "message_body").(map[string]any)
		if !ok {
			return false, errors.New("message lookup missing body")
		}
		for key, expected := range map[string]string{"conversation_id": conversation, "message_id": messageID} {
			if value := textValue(part[key]); value != "" && value != expected {
				return false, errors.New("message lookup identity mismatch")
			}
		}
		if message == nil || name == "FULL_MSG" {
			message = part
		} else {
			// A changed or incomplete fragment metadata record needs the full
			// conversation check rather than the first fragment's stale values.
			for _, key := range []string{"user_type", "bot_reply_message_id", "status", "status_v2", "content_status", "final_status", "ext"} {
				if value, exists := part[key]; exists && !reflect.DeepEqual(message[key], value) {
					return false, errors.New("message lookup fragment metadata changed")
				}
			}
			// The native loader preserves the first fragment's complete metadata
			// and appends later content blocks. A split body never bypasses the
			// caller's final identity and status checks.
			message["content_block"] = append(array(message["content_block"]), array(part["content_block"])...)
		}
		terminal = name == "FULL_MSG" || name == "STREAM_MSG_END"
		return terminal, nil
	})
	if err != nil {
		return nil, err
	}
	if !terminal || message == nil {
		return nil, errors.New("message lookup ended without terminal snapshot")
	}
	return message, nil
}
