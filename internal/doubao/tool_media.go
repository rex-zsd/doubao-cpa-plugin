package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type toolMediaScopeKey struct{}
type toolMediaScope struct {
	model                    Model
	taskID, prompt, nativeID string
	suspending               func() bool
}

func (g *gateway) toolReply(ctx context.Context, t *gatewayTurn, native object, value string, batch bool) (object, error) {
	g.mu.Lock()
	req := t.request
	isError := false
	for _, p := range g.calls {
		if p.turn == t && p.nativeID == textValue(native["id"]) {
			isError = p.isError
			break
		}
	}
	g.mu.Unlock()
	model, _, _ := selectModel(g.models, req.Model, "work")
	// The auxiliary reader sees text context, never inline base64 as prompt text.
	contextMessages := []apiMessage{}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			continue
		}
		parts, ok := m.Content.([]any)
		if ok {
			texts := []string{}
			for _, part := range parts {
				if field(part, "type") == "text" {
					texts = append(texts, textValue(field(part, "text")))
				}
			}
			m.Content = strings.Join(texts, "\n")
		}
		m.ToolCalls = nil
		contextMessages = append(contextMessages, m)
	}
	scope := toolMediaScope{model: model, taskID: t.id, prompt: jsonText(contextMessages), nativeID: textValue(native["id"]), suspending: func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.suspending }}
	reply, err := g.c.nativeToolReply(context.WithValue(ctx, toolMediaScopeKey{}, scope), native, value, batch, isError)
	if err == nil && (native["name"] == "Read" || native["name"] == "Bash" || native["name"] == "Write" || native["name"] == "Edit") {
		// Builtins use content/isError directly; raw_result belongs to MCP.
		content := field(parsed(reply["raw_result"]), "content")
		delete(reply, "raw_result")
		reply["content"], reply["isError"] = content, isError
		if isError {
			reply["error_code"], reply["error_msg"] = "EXTERNAL_TOOL_ERROR", value
		}
	}
	return reply, err
}
func (g *gateway) awaitNativePending(ctx context.Context, t *gatewayTurn, p *clientToolPending, native object) (object, error) {
	select {
	case value := <-p.result:
		return g.toolReply(ctx, t, native, value, len(p.batchCalls) > 0)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Work's raw MCP media results are not a verified vision input. A separate,
// journaled media-reading task supplies textual observations to the original
// agent. This costs an additional generation and is explicitly documented.
func (c *Client) observeToolMedia(ctx context.Context, scope toolMediaScope, parts []any) (string, error) {
	path := filepath.Join(c.dir, "gateway-media-"+scope.taskID+"-"+apiHash(object{"native_id": scope.nativeID, "content": parts})[:24]+".json")
	var receipt Receipt
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &receipt) != nil {
			return "", errors.New("invalid media observation receipt")
		}
		if receipt.Completed {
			return receipt.Reply, nil
		}
		if receipt.Dispatched && !receipt.Accepted {
			return "", apiFail(409, "media_observation_unconfirmed", "Media reader acceptance is unconfirmed; observation was not resubmitted")
		}
		if receipt.Accepted {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				text, done, err := c.readReply(ctx, &receipt)
				if err != nil {
					return "", err
				}
				if done {
					receipt.Completed, receipt.Reply = true, text
					if err = saveJSON(path, &receipt); err != nil {
						return "", err
					}
					return text, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-ticker.C:
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	prompt := "这些附件来自调用方已经执行完毕的外部工具。请实际读取每一个附件，提取下面任务需要的、可从附件核对的事实，保留名称、数量、字段与数值。只输出读取到的事实；不能执行函数，也不能要求再次获取附件。无法辨认的内容请明确标注，不要猜测。任务上下文：\n" + scope.prompt
	content := append([]any{object{"type": "text", "text": prompt}}, parts...)
	request := apiRequest{Messages: []apiMessage{{Role: "user", Content: content}}}
	prepared, records, err := c.prepareAPIMedia(ctx, request)
	if err != nil {
		return "", err
	}
	body := buildBody(textValue(prepared.Messages[0].Content), "", "work", scope.model)
	appendMedia(body, records)
	r, err := c.run(ctx, body, path)
	if err != nil {
		if r != nil && r.Accepted && !r.Completed && ctx.Err() != nil && (scope.suspending == nil || !scope.suspending()) {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = c.stopProbe(stopCtx, path, object{"action": "tool-stop"})
			cancel()
		}
		return "", err
	}
	if r == nil || !r.Completed {
		return "", errors.New("media reader did not complete")
	}
	return r.Reply, nil
}
