package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func exchangeRecord(x *apiExchange) any {
	if x == nil {
		return nil
	}
	record := object{"id": x.id, "model": x.model, "created": x.created, "prompt_tokens": x.prompt}
	select {
	case <-x.ready:
		record["resolved"], record["message"], record["finish"], record["error"], record["error_status"] = true, x.message, x.finish, x.err, errorStatus(x.err)
	default:
	}
	return record
}
func restoreExchange(v any, model string) *apiExchange {
	x := newExchange(model)
	if id := textValue(field(v, "id")); id != "" {
		x.id = id
	}
	if created := number(field(v, "created")); created != 0 {
		x.created = int64(created)
	}
	x.prompt = number(field(v, "prompt_tokens"))
	if field(v, "resolved") == true || field(v, "message") != nil {
		var message apiMessage
		_ = json.Unmarshal([]byte(jsonText(field(v, "message"))), &message)
		x.resolve(message, textValue(field(v, "finish")), restoreError(field(v, "error"), number(field(v, "error_status"))))
	}
	return x
}
func restoreError(v any, status int) *apiError {
	if v == nil {
		return nil
	}
	e := &apiError{}
	if json.Unmarshal([]byte(jsonText(v)), e) != nil {
		return apiFail(409, "task_interrupted", "Persisted task failed; no generation was recreated")
	}
	if status == 0 {
		status = 409
	}
	e.Status = status
	return e
}
func (g *gateway) findNativePending(t *gatewayTurn, id string) *clientToolPending {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.calls {
		if p.turn == t && p.nativeID == id {
			return p
		}
	}
	return nil
}

// Recovery never POSTs a generation. Ambiguous result writes fail closed.
func (g *gateway) recoverTasks() error {
	paths, err := filepath.Glob(filepath.Join(g.dir, "gateway-state-*.json"))
	if err != nil {
		return err
	}
	active, cleanup := []*gatewayTurn{}, []*gatewayTurn{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var state object
		if err = decode(strings.NewReader(string(data)), &state); err != nil {
			return err
		}
		id := textValue(state["id"])
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" || filepath.Base(path) != "gateway-state-"+id+".json" {
			return errors.New("invalid persisted task identity")
		}
		var req apiRequest
		if json.Unmarshal([]byte(jsonText(state["request"])), &req) != nil {
			return errors.New("invalid persisted request")
		}
		deadline := time.Unix(int64(number(state["deadline"])), 0)
		if number(state["deadline"]) <= 0 {
			info, _ := os.Stat(path)
			deadline = info.ModTime().Add(g.taskTimeout)
		}
		ctx, cancel := context.WithCancel(g.ctx)
		t := &gatewayTurn{id: id, request: req, ctx: ctx, cancel: cancel, deadline: deadline, statePath: path, receiptPath: filepath.Join(g.dir, "gateway-receipt-"+id+".json"), connector: textValue(state["connector_id"]), sandbox: textValue(state["sandbox_id"]), generationPosts: number(state["generation_posts"]), forwarded: number(state["forwarded_tools"]), denied: number(state["denied_tools"]), cleanupConfirmed: state["cleanup_confirmed"] == true, cleanupRequested: state["cleanup_requested"] == true, initialKey: textValue(state["initial_key"]), initialHash: textValue(state["initial_hash"]), finalText: textValue(state["final_text"]), conversation: textValue(state["conversation_id"]), mode: textValue(state["mode"]), workspace: textValue(state["workspace"]), continued: number(state["continued"])}
		exchanges := map[string]*apiExchange{}
		restore := func(v any) *apiExchange {
			key := textValue(field(v, "id"))
			if x := exchanges[key]; key != "" && x != nil {
				return x
			}
			x := restoreExchange(v, req.Model)
			exchanges[x.id] = x
			return x
		}
		t.first, t.current = restore(state["first_exchange"]), restore(state["current_exchange"])
		groups := map[string]*clientToolPending{}
		for _, item := range array(state["pending"]) {
			nativeID := textValue(field(item, "native_id"))
			if nativeID == "" {
				continue
			}
			p := groups[nativeID]
			if p == nil {
				p = &clientToolPending{turn: t, nativeID: nativeID, result: make(chan string, 1), submitted: field(item, "submitted") == true, value: textValue(field(item, "result"))}
				_ = json.Unmarshal([]byte(jsonText(field(item, "call"))), &p.call)
				_ = json.Unmarshal([]byte(jsonText(field(item, "batch_calls"))), &p.batchCalls)
				_ = json.Unmarshal([]byte(jsonText(field(item, "expected_messages"))), &p.expected)
				if len(p.expected) == 0 {
					return errors.New("persisted tool missing history")
				}
				p.exposed = restore(field(item, "exchange"))
				p.exposed.resolve(p.expected[len(p.expected)-1], "tool_calls", nil)
				if field(item, "next_exchange") != nil {
					p.next = restore(field(item, "next_exchange"))
				}
				groups[nativeID] = p
			}
			g.calls[textValue(field(item, "id"))] = p
		}
		ordered := []*clientToolPending{}
		for _, p := range groups {
			ordered = append(ordered, p)
		}
		sort.Slice(ordered, func(i, j int) bool { return len(ordered[i].expected) < len(ordered[j].expected) })
		for i, p := range ordered {
			if p.submitted {
				if i+1 < len(ordered) {
					p.next = ordered[i+1].exposed
				} else {
					p.next = t.current
				}
				p.result <- p.value
			} else {
				t.current = p.exposed
			}
		}
		if state["done"] == true {
			t.done = true
			t.finished = time.Unix(int64(number(state["finished_at"])), 0)
			if number(state["finished_at"]) <= 0 {
				t.finished = time.Now()
			}
			t.err = restoreError(state["error"], number(state["error_status"]))
			t.current.resolve(apiMessage{Role: "assistant", Content: t.finalText}, "stop", t.err)
			cancel()
		} else {
			var receipt Receipt
			b, readErr := os.ReadFile(t.receiptPath)
			uncertain := readErr != nil || json.Unmarshal(b, &receipt) != nil || !receipt.Accepted
			var journal map[string]object
			if b, readErr = os.ReadFile(filepath.Join(g.dir, "gateway-native-"+id+".json")); readErr == nil {
				if json.Unmarshal(b, &journal) != nil {
					return errors.New("invalid native tool journal")
				}
				for _, row := range journal {
					if row["uploaded"] != true && row["upload_body"] != nil {
						uncertain = true
					}
				}
			}
			var failure *apiError
			switch {
			case t.generationPosts == 0:
				failure = apiFail(409, "task_not_submitted", "Host stopped before generation submission; no native task was recreated")
			case uncertain:
				failure = apiFail(409, "native_write_unconfirmed", "Native write outcome is unconfirmed after restart; generation/result was not resent")
			case !deadline.After(time.Now()):
				failure = apiFail(504, "task_expired", "Original task deadline expired; no generation was recreated")
			}
			if failure != nil {
				t.done, t.finished, t.err = true, time.Now(), failure
				t.current.resolve(apiMessage{}, "", failure)
				cancel()
				if err = g.checkpointLocked(t); err != nil {
					return err
				}
			} else {
				active = append(active, t)
			}
		}
		g.turns[id] = t
		if t.initialKey != "" {
			g.idempotency[t.initialKey] = gatewayIdempotency{hash: t.initialHash, turn: t, exchange: t.first}
		}
		if t.done && ((t.connector != "" && !t.cleanupConfirmed) || t.err != nil && t.err.Code == "task_expired") {
			cleanup = append(cleanup, t)
		}
	}
	// Threads live in memory, so after a restart only recovered tasks still
	// use a task folder.
	folders, _ := filepath.Glob(filepath.Join(g.dir, "gateway-workspace-*"))
	for _, folder := range folders {
		g.releaseWorkspaceLocked(folder)
	}
	for _, t := range active {
		g.armLocked(t)
		g.wg.Add(1)
		go func(t *gatewayTurn) { defer g.wg.Done(); g.resumeNative(t) }(t)
	}
	for _, t := range cleanup {
		g.wg.Add(1)
		go func(t *gatewayTurn) { defer g.wg.Done(); g.stopExpiredReceipts(t); _ = g.removeConnector(t) }(t)
	}
	return nil
}
func (g *gateway) resumeNative(t *gatewayTurn) {
	var receipt Receipt
	b, err := os.ReadFile(t.receiptPath)
	if err == nil {
		err = json.Unmarshal(b, &receipt)
	}
	if err != nil {
		g.finish(t, "", apiFail(409, "invalid_receipt", "Cannot recover native receipt"))
		return
	}
	ctx, cancel := context.WithCancel(t.ctx)
	var streamDone <-chan error
	streamJoined := false
	defer func() {
		cancel()
		if streamDone != nil && !streamJoined {
			<-streamDone
		}
	}()
	if t.connector != "" {
		workspace := turnWorkspace(g.dir, t)
		if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
			workspace = resolved
		}
		p := &toolProbe{c: g.c, sandbox: t.sandbox, workspace: workspace, connector: t.connector, journal: filepath.Join(g.dir, "gateway-native-"+t.id+".json"), events: map[string]int{}, records: map[string]object{}, forward: func(ctx context.Context, call, input object) (object, error) {
			return g.forwardClientTool(ctx, t, call, input)
		}}
		var saved map[string]object
		if b, err := os.ReadFile(p.journal); err == nil {
			_ = json.Unmarshal(b, &saved)
		}
		pending := []object{}
		for key, row := range saved {
			if row["uploaded"] == true {
				p.records[key] = row
			} else if frame, ok := row["frame"].(map[string]any); ok {
				pending = append(pending, frame)
			}
		}
		streamDone = g.serveTools(ctx, ctx, cancel, p, pending)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		reply, done, readErr := g.c.readReply(ctx, &receipt)
		if readErr != nil {
			err = readErr
			break
		}
		if done {
			receipt.Completed, receipt.Reply = true, reply
			_ = saveJSON(t.receiptPath, &receipt)
			cancel()
			_ = g.removeConnector(t)
			g.mu.Lock()
			t.conversation = receipt.ConversationID
			g.mu.Unlock()
			g.finish(t, reply, nil)
			return
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case streamErr := <-streamDone:
			streamJoined = true
			err = streamErr
			if err == nil {
				err = errors.New("native stream ended")
			}
		case <-ticker.C:
			continue
		}
		break
	}
	cancel()
	g.mu.Lock()
	suspending := g.suspending
	g.mu.Unlock()
	if suspending {
		return
	}
	if receipt.Accepted && !receipt.Completed {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		_ = g.c.stopProbe(stopCtx, t.receiptPath, object{"action": "tool-stop"})
		stop()
	}
	_ = g.removeConnector(t)
	g.mu.Lock()
	failure := t.abort
	g.mu.Unlock()
	if failure == nil {
		failure = apiFail(409, "recovery_interrupted", "Original task could not be resumed; no new generation was created")
	}
	g.finish(t, "", failure)
	_ = err
}

func (g *gateway) stopExpiredReceipts(t *gatewayTurn) {
	if t.err == nil || t.err.Code != "task_expired" {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(g.dir, "gateway-media-"+t.id+"-*.json"))
	paths = append(paths, t.receiptPath)
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var receipt Receipt
		if json.Unmarshal(b, &receipt) != nil || !receipt.Accepted || receipt.Completed {
			continue
		}
		ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
		_ = g.c.stopProbe(ctx, path, object{"action": "tool-stop"})
		cancel()
	}
}
