package doubao

import (
	"strings"
)

// Only root answer text is eligible. Unknown, collapsed, hidden and parented
// blocks are withheld; final IM readback verifies everything already emitted.
type nativeVisibleStream struct {
	runID, messageID string
	blocks           map[string]object
	order            []string
}

func (s *nativeVisibleStream) observe(name string, data object) string {
	if s.blocks == nil {
		s.blocks = map[string]object{}
	}
	if name == "SSE_ACK" {
		queries := array(data["query_list"])
		if len(queries) > 0 {
			s.runID = textValue(field(queries[0], "question_id"))
		}
		return ""
	}
	var changes []any
	switch name {
	case "STREAM_MSG_NOTIFY":
		meta := data["meta"]
		if number(field(meta, "user_type")) != 2 || s.runID == "" || textValue(field(meta, "bot_reply_message_id")) != s.runID {
			return ""
		}
		id := textValue(field(meta, "message_id"))
		if s.messageID != "" && s.messageID != id {
			return ""
		}
		s.messageID = id
		changes = array(field(data, "content", "content_block"))
	case "STREAM_CHUNK":
		if s.messageID == "" || textValue(data["message_id"]) != s.messageID {
			return ""
		}
		for _, op := range array(data["patch_op"]) {
			changes = append(changes, array(field(op, "patch_value", "content_block"))...)
		}
	default:
		return ""
	}
	for _, v := range changes {
		incoming, ok := v.(map[string]any)
		if !ok {
			continue
		}
		id := textValue(incoming["block_id"])
		if id == "" {
			continue
		}
		previous := s.blocks[id]
		if previous == nil {
			previous = object{}
			s.blocks[id] = previous
			s.order = append(s.order, id)
		}
		if text, ok := field(incoming, "content", "text_block", "text").(string); ok {
			if number(incoming["patch_type"]) == 1 {
				previous["answer_text"] = textValue(previous["answer_text"]) + text
			} else {
				previous["answer_text"] = text
			}
		}
		for _, key := range []string{"block_type", "parent_id", "control_info"} {
			if value, ok := incoming[key]; ok {
				previous[key] = value
			}
		}
		if field(incoming, "content", "thinking_block") != nil {
			previous["hidden"] = true
		}
	}
	texts := []string{}
	for _, id := range s.order {
		b := s.blocks[id]
		collapse := textValue(field(b, "control_info", "collapse_block_id"))
		// Work temporarily groups the visible root answer under its elapsed-time
		// indicator. This known UI container is distinct from reasoning parents.
		unknownCollapse := collapse != "" && number(field(s.blocks[collapse], "block_type")) != 10091
		if number(b["block_type"]) != 10000 || b["hidden"] == true || textValue(b["parent_id"]) != "" || unknownCollapse || field(b, "control_info", "is_visible") == false || field(b, "control_info", "is_collapsed") == true {
			continue
		}
		if text := textValue(b["answer_text"]); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}
func (x *apiExchange) updateVisible(text string) {
	if text == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if text == x.visible || !strings.HasPrefix(text, x.visible) {
		return
	}
	x.visible = text
	close(x.changed)
	x.changed = make(chan struct{})
}
func (x *apiExchange) visibleSnapshot() (string, <-chan struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.visible, x.changed
}
func (g *gateway) answerObserver(t *gatewayTurn) func(string, object) error {
	state := &nativeVisibleStream{}
	return func(name string, data object) error {
		text := state.observe(name, data)
		g.mu.Lock()
		x := t.current
		// Tool boundaries are atomic. Do not leak Work plans or commentary into the
		// full-history function protocol before its final readback is confirmed.
		// JSON answers stream whole, since the final one may lose a fence.
		enabled := !usesTools(t.request) && !jsonMode(t.request)
		g.mu.Unlock()
		if enabled {
			x.updateVisible(text)
		}
		return nil
	}
}
