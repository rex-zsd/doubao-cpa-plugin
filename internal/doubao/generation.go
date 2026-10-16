package doubao

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Model struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	Think     int      `json:"think"`
	Available bool     `json:"available"`
	Modes     []string `json:"modes"`
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func selectModel(models []Model, requestedName, requestedMode string) (Model, string, error) {
	modes := []string{requestedMode}
	if requestedMode == "auto" {
		modes = []string{"chat", "work"}
	}
	for _, mode := range modes {
		for _, model := range models {
			if model.Available && contains(model.Modes, mode) && (requestedName == "" || model.Name == requestedName) {
				return model, mode, nil
			}
		}
	}
	return Model{}, "", errors.New("requested model is unavailable for the selected mode")
}
func (c *Client) models(ctx context.Context) ([]Model, error) {
	v, err := c.json(ctx, "POST", "/alice/slot/action_bar_v3/brief_list", nil, object{"language_code": "zh", "bot_id": botID})
	if err != nil {
		return nil, err
	}
	if number(field(v, "code")) != 0 {
		return nil, errors.New("model catalog rejected")
	}
	var config any
	for _, entry := range array(field(v, "data", "entry_list")) {
		if textValue(field(entry, "action_bar_key")) == "coco_deep_thinking" {
			config = field(entry, "active_switch_conf")
		}
	}
	byID := map[string]*Model{}
	add := func(item any, mode string) {
		id := textValue(field(item, "model_item_key"))
		if id == "" {
			id = textValue(field(item, "item_id"))
		}
		name := textValue(field(item, "name"))
		if id == "" || name == "" {
			return
		}
		m := byID[id]
		if m == nil {
			m = &Model{ID: id, Name: name, Provider: textValue(field(item, "model_extra_params", "provider_id")), Think: number(field(item, "item_id")), Available: !truthy(field(item, "disabled")) && !truthy(field(item, "subscribe_config", "need_upgrade")), Modes: []string{}}
			byID[id] = m
			if n, e := strconv.Atoi(id); e == nil {
				m.Think = n
			}
			if textValue(field(item, "model_extra_params", "model_type")) == "custom" {
				m.Think = 10001
			}
		}
		if !contains(m.Modes, mode) {
			m.Modes = append(m.Modes, mode)
		}
	}
	for _, mode := range array(field(config, "menu_conf_v2", "mode_list", "item_list")) {
		label := ""
		if number(field(mode, "agent_mode")) == 1 {
			label = "work"
		}
		if number(field(mode, "agent_mode")) == 2 {
			label = "chat"
		}
		if label == "" {
			continue
		}
		for _, item := range array(field(mode, "model_list", "item_list")) {
			add(item, label)
		}
		for _, item := range array(field(config, "menu_conf_v2", "model_list", "item_list")) {
			supported := array(field(mode, "support_models"))
			eligible := len(supported) == 0
			for _, allowed := range supported {
				id := textValue(field(allowed, "model_item_key"))
				if id == "" {
					id = textValue(field(allowed, "item_id"))
				}
				if id == "" {
					id = textValue(allowed)
				}
				if id == textValue(field(item, "model_item_key")) || id == textValue(field(item, "item_id")) {
					eligible = true
				}
			}
			if eligible {
				add(item, label)
			}
		}
	}
	if len(byID) == 0 {
		for _, item := range array(field(config, "menu_conf", "item_list")) {
			mode := "chat"
			if number(field(item, "agent_mode")) == 1 {
				mode = "work"
			}
			add(item, mode)
		}
	}
	models := []Model{}
	for _, m := range byID {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	if len(models) == 0 {
		return nil, errors.New("empty model catalog")
	}
	return models, nil
}

// An SSE decoder independent of network read chunk boundaries. Pending frames at EOF are errors.
func consumeSSE(r io.Reader, callback func(string, object, string) (bool, error)) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 4<<20)
	name, id := "", ""
	lines := []string{}
	size := 0
	for s.Scan() {
		line := strings.TrimSuffix(s.Text(), "\r")
		size += len(line)
		if size > 4<<20 {
			return errors.New("SSE frame too large")
		}
		if line == "" {
			if len(lines) > 0 {
				if name == "" {
					name = "message"
				}
				var payload object
				if err := decode(strings.NewReader(strings.Join(lines, "\n")), &payload); err != nil {
					return errors.New("invalid SSE JSON")
				}
				stop, err := callback(name, payload, id)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
			name, id, lines, size = "", "", nil, 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			name = value
		case "id":
			id = value
		case "data":
			lines = append(lines, value)
		}
	}
	if err := s.Err(); err != nil {
		return errors.New("SSE read failed")
	}
	if name != "" || len(lines) > 0 {
		return errors.New("SSE ended inside an event")
	}
	return nil
}

type task struct {
	ID        string `json:"id"`
	Seq       int    `json:"seq"`
	Scene     any    `json:"scene"`
	Completed bool   `json:"completed"`
}
type Receipt struct {
	ConversationID string         `json:"conversation_id"`
	RunID          string         `json:"run_id"`
	LocalID        string         `json:"local_id"`
	Accepted       bool           `json:"accepted"`
	Dispatched     bool           `json:"dispatched"`
	StreamEnded    bool           `json:"stream_ended"`
	Tasks          []*task        `json:"tasks"`
	Events         map[string]int `json:"events"`
	Reply          string         `json:"reply,omitempty"`
	Completed      bool           `json:"completed"`
	Request        object         `json:"request"`
}

func buildBody(message, conversation, mode string, model Model) object {
	localID := token()
	work := mode == "work"
	agent, modeID := 2, "1"
	if work {
		agent, modeID = 1, "3"
	}
	gt := object{"action": 0, "thread_local_message_id": []string{localID}, "runtime_type": 1, "agent_task_param": object{"runtime_type": 1}, "agent_task_param_change": object{"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false}, "need_modify_conversation": false, "task_input_json": `{"schema_version":1,"project_context":{}}`}
	init := object{"need_ack_conversation": true}
	option := object{"create_time_ms": time.Now().UnixMilli(), "agent_mode": agent, "need_deep_think": model.Think, "unique_key": token(), "recovery_option": object{"is_recovery": false, "req_create_time_sec": time.Now().Unix(), "append_sse_event_scene": 0}, "need_create_conversation": conversation == "", "is_old_user": true, "message_from": 0, "sse_recv_event_options": object{"support_chunk_delta": true}, "general_task_param": gt, "model_config": object{"model_item_key": model.ID}, "aggregate_params": object{"provider_id": model.Provider}}
	if conversation == "" {
		option["conversation_init_option"] = init
		option["conversation_init_ext"] = object{"mode_id": modeID, "model_item_key": model.ID}
	}
	return object{"client_meta": object{"local_conversation_id": "poc_" + token(), "conversation_id": conversation, "bot_id": botID, "last_section_id": "", "last_message_index": nil}, "messages": []any{object{"local_message_id": localID, "content_block": []any{object{"block_type": 10000, "content": object{"text_block": object{"text": message}}, "block_id": token()}}, "message_status": 0}}, "option": option, "ext": object{"general_task_param": jsonText(gt), "use_deep_think": strconv.Itoa(model.Think), "agent_mode": strconv.Itoa(agent), "sub_conv_firstmet_type": "1", "collection_id": "", "is_finish": "1", "conversation_init_option": jsonText(init), "commerce_credit_config_enable": "0"}, "user_context": []any{}}
}
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (c *Client) run(ctx context.Context, body object, receiptPath string) (*Receipt, error) {
	return c.runObserved(ctx, body, receiptPath, nil)
}
func (c *Client) runObserved(ctx context.Context, body object, receiptPath string, observe func(string, object) error) (*Receipt, error) {
	local := textValue(field(array(body["messages"])[0], "local_message_id"))
	r := &Receipt{ConversationID: textValue(field(body, "client_meta", "conversation_id")), LocalID: local, Tasks: []*task{}, Events: map[string]int{}, Request: body}
	checkpoint := func() error { return saveJSON(receiptPath, r) }
	if err := checkpoint(); err != nil {
		return r, err
	}
	r.Dispatched = true
	if err := checkpoint(); err != nil {
		return r, err
	}
	response, err := c.request(ctx, "POST", "/chat/completion", nil, body)
	if err != nil {
		return r, err
	}
	callback := func(name string, data object, _ string) (bool, error) {
		r.Events[name]++
		if observe != nil {
			if err := observe(name, data); err != nil {
				return false, err
			}
		}
		switch name {
		case "SSE_ACK":
			id := textValue(field(data, "ack_client_meta", "conversation_id"))
			queries := array(field(data, "query_list"))
			if len(queries) == 0 {
				return false, errors.New("ACK missing query")
			}
			run := textValue(field(queries[0], "question_id"))
			receivedLocal := textValue(field(queries[0], "local_message_id"))
			if id == "" || run == "" || r.ConversationID != "" && r.ConversationID != id || receivedLocal != "" && receivedLocal != local {
				return false, errors.New("ACK identity mismatch")
			}
			r.ConversationID, r.RunID, r.Accepted = id, run, true
		case "FETCH_STREAM":
			if number(data["fetch_type"]) == 2 && textValue(data["fetch_key"]) != "" {
				id := textValue(data["fetch_key"])
				exists := false
				for _, t := range r.Tasks {
					if t.ID == id {
						exists = true
					}
				}
				if !exists {
					if len(r.Tasks) >= 16 {
						return false, errors.New("task count limit")
					}
					r.Tasks = append(r.Tasks, &task{ID: id, Scene: data["append_scene"]})
				}
				return true, checkpoint()
			}
		case "SSE_REPLY_END":
			if number(data["end_type"]) == 3 {
				r.StreamEnded = true
				return true, checkpoint()
			}
		case "STREAM_ERROR", "gateway-error":
			return false, errors.New("upstream stream error")
		}
		return false, checkpoint()
	}
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body.Close()
		return r, errors.New("completion response is not SSE")
	}
	err = consumeSSE(response.Body, callback)
	response.Body.Close()
	if err != nil {
		return r, err
	}
	if !r.Accepted {
		return r, errors.New("no verified ACK; acceptance unknown; do not resend")
	}
	for i := 0; i < len(r.Tasks); i++ {
		t := r.Tasks[i]
		response, err = c.request(ctx, "POST", "/chat/async/chunk_stream", nil, object{"task_id": t.ID, "seq_start": t.Seq, "append_scene": t.Scene, "ext": object{}})
		if err != nil {
			return r, err
		}
		err = consumeSSE(response.Body, func(name string, data object, id string) (bool, error) {
			seq := number(id)
			if seq > 0 && seq <= t.Seq {
				return false, nil
			}
			if seq > t.Seq {
				t.Seq = seq
			}
			if name == "ASYNC_CHUNK_SNAPSHOT" && number(data["next_seq"]) > t.Seq {
				t.Seq = number(data["next_seq"])
			}
			if name == "SSE_REPLY_END" && number(data["end_type"]) == 3 {
				t.Completed = true
			}
			stop, e := callback(name, data, id)
			return stop, e
		})
		response.Body.Close()
		if err != nil {
			return r, err
		}
		if !t.Completed {
			return r, errors.New("async task has no terminal event; do not resend")
		}
	}
	for {
		text, finished, err := c.readReply(ctx, r)
		if err != nil {
			return r, err
		}
		if finished {
			r.Reply = text
			r.Completed = true
			return r, checkpoint()
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
func visibleText(message any) string {
	blocks := array(field(message, "content_blocks_v2"))
	if len(blocks) == 0 {
		blocks = array(field(message, "content_block"))
	}
	if len(blocks) == 0 {
		return textValue(field(parsed(field(message, "content")), "text"))
	}
	hidden := map[string]bool{}
	for _, b := range blocks {
		if field(b, "content", "thinking_block") != nil {
			hidden[textValue(field(b, "block_id"))] = true
		}
	}
	for n := 0; n < len(blocks); n++ {
		for _, b := range blocks {
			if hidden[textValue(field(b, "parent_id"))] {
				hidden[textValue(field(b, "block_id"))] = true
			}
		}
	}
	texts := []string{}
	for _, b := range blocks {
		if hidden[textValue(field(b, "block_id"))] || truthy(field(b, "control_info", "collapse_block_id")) {
			continue
		}
		if t := textValue(field(b, "content", "text_block", "text")); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}
func (c *Client) readReply(ctx context.Context, r *Receipt) (string, bool, error) {
	v, err := c.json(ctx, "POST", "/im/conversation/batch_get", nil, object{"cmd": 1111, "uplink_body": object{"batch_get_conv_info_uplink_body": object{"conversation_id": []string{r.ConversationID}, "option": object{"recent_message_count_per_conv": 100}, "ext": object{}}}, "sequence_id": token(), "channel": 2, "version": "1"})
	if err != nil {
		return "", false, err
	}
	if number(v["status_code"]) != 0 {
		if err := saveJSON(filepath.Join(c.dir, "readback-rejected.json"), v); err != nil {
			return "", false, err
		}
		return "", false, fmt.Errorf("readback rejected, status_code=%d", number(v["status_code"]))
	}
	for _, conv := range array(field(v, "downlink_body", "batch_get_conv_info_downlink_body", "conversation_info_list")) {
		if textValue(field(conv, "conversation_id")) != r.ConversationID {
			continue
		}
		messages := array(field(conv, "messages"))
		sort.SliceStable(messages, func(i, j int) bool {
			a, _ := strconv.ParseUint(textValue(field(messages[i], "message_id")), 10, 64)
			b, _ := strconv.ParseUint(textValue(field(messages[j], "message_id")), 10, 64)
			return a < b
		})
		root := false
		var latest any
		for _, m := range messages {
			if number(field(m, "user_type")) == 1 && textValue(field(m, "message_id")) == r.RunID {
				root = true
			}
			parent := textValue(field(m, "bot_reply_message_id"))
			if parent == "" {
				parent = textValue(field(m, "ext", "chat_id"))
			}
			if number(field(m, "user_type")) == 2 && parent == r.RunID && number(field(m, "status")) == 0 {
				latest = m
			}
		}
		if !root || latest == nil {
			return "", false, nil
		}
		if number(field(latest, "content_status")) == 500 || truthy(field(latest, "ext", "error_details")) {
			return "", false, errors.New("server reports failed generation")
		}
		if textValue(field(latest, "ext", "is_interrupted")) == "true" || number(field(latest, "content_status")) == 120 {
			return "", false, errors.New("server reports cancelled generation")
		}
		blocks := array(field(latest, "content_block"))
		for _, b := range blocks {
			if field(b, "content", "complex_task_block") != nil || field(b, "content", "task_card_block") != nil {
				return "", false, nil
			}
			if number(field(b, "content", "interaction_ask_block", "status")) == 1 {
				return "", false, errors.New("waiting_input")
			}
		}
		answer := visibleText(latest)
		if answer != "" && textValue(field(latest, "ext", "is_finish")) == "1" {
			return answer, true, nil
		}
		return "", false, nil
	}
	return "", false, nil
}

// Keep source-only bundles free of private sessions and request receipts.
func writeEvidence(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return saveJSON(path, value)
}
