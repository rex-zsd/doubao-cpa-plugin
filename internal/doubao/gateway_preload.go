package doubao

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Work takes at most maxPromptChars per message, but one native conversation
// holds far more: on 2026-11-16 gpt-6.1-sol recalled markers from every one
// of four 225K-character messages (Work reported ~500K tokens of context).
// A request whose history does not fit one message therefore sends its older
// history first, in segments, into the conversation that then answers it.
//
// Nothing is dropped silently: Work truncates a conversation past its window
// (its reported context falls back or stops growing), so every segment must
// raise the context Work reports by a plausible share of its size, else the
// request fails with the usual context_length_exceeded.
const (
	preloadMaxSegments = 8
	// preloadMinGrowth is the least context growth per segment, in percent of
	// its estimated tokens. Real growth measured 200% (hex logs); truncation
	// showed a drop or under 1%.
	preloadMinGrowth = 40
	// preloadMargin leaves room for the short answers rendered around a
	// segment and for Work counting slightly differently.
	preloadMargin          = 4096
	preloadToolRounds      = 2
	preloadMediaPerSegment = 12
)

type preloadPlan struct {
	// base messages already live in a native conversation; split is the first
	// message sent with the request itself.
	base, split int
	context     int
	segments    [][]any
	texts       []string
}

// submitLong is submit, plus segmented history for requests too long for one
// Work message.
func (g *gateway) submitLong(ctx context.Context, req apiRequest, key string) (*apiExchange, *apiError) {
	x, failure := g.submit(req, key)
	if failure == nil || failure.Code != "context_length_exceeded" || req.N > 1 {
		return x, failure
	}
	plan, reason := g.preloadPlan(req)
	if reason != "" {
		fmt.Fprintf(os.Stderr, "GATEWAY_PRELOAD_SKIPPED reason=%s messages=%d\n", reason, len(req.Messages))
		return nil, failure
	}
	if reason = g.preload(ctx, req, plan); reason != "" {
		fmt.Fprintf(os.Stderr, "GATEWAY_PRELOAD_FAILED reason=%s\n", reason)
		return nil, failure
	}
	return g.submit(req, key)
}

// peekThreadLocked is claimThreadLocked without claiming: how many leading
// messages a live conversation already holds, and its context. g.mu held.
func (g *gateway) peekThreadLocked(req apiRequest) (int, int) {
	header, keys := threadHeader(req), threadKeys(req.Messages)
	for i := len(req.Messages) - 2; i >= 0; i-- {
		if m := req.Messages[i]; m.Role != "assistant" || len(m.ToolCalls) > 0 {
			continue
		}
		th := g.threads[threadKey(header, keys[i])]
		if th == nil || th.mode == "tools" && th.nativeTools != g.nativeTools || time.Since(th.used) > threadTTL {
			continue
		}
		return i + 1, th.context
	}
	return 0, 0
}

// preloadPlan picks the longest tail that fits one message and starts after
// a plain answer (where a conversation can continue), and cuts the history
// before it into segments.
func (g *gateway) preloadPlan(req apiRequest) (preloadPlan, string) {
	g.mu.Lock()
	base, known := g.peekThreadLocked(req)
	g.mu.Unlock()
	plan := preloadPlan{base: base, split: -1, context: known}
	for c := len(req.Messages) - 1; c > base; c-- {
		if m := req.Messages[c-1]; m.Role != "assistant" || len(m.ToolCalls) > 0 {
			continue
		}
		if utf16Len(outgoingPrompt(req, c)) > maxPromptChars {
			break
		}
		plan.split = c
	}
	if plan.split < 0 {
		return plan, "tail_too_long"
	}
	history, media := preloadHistory(req.Messages[plan.base:plan.split], plan.base)
	// Each segment is rendered alone; measure what the rendering adds.
	overhead := utf16Len(outgoingPrompt(segmentRequest(req, nil, []any{object{"type": "text", "text": ""}}), 0))
	budget := maxPromptChars - overhead - utf16Len(preloadHeader(99, 99)) - preloadMargin
	if budget < maxPromptChars/4 {
		return plan, "overhead"
	}
	for history != "" {
		size := budget
		for {
			chunk := cutHistory(history, size)
			parts, rest := segmentParts(chunk, media)
			if len(parts) > preloadMediaPerSegment+1 {
				size = size * 9 / 10
				continue
			}
			if utf16Len(outgoingPrompt(segmentRequest(req, nil, parts), 0)) > maxPromptChars-preloadMargin {
				size = size * 9 / 10
				if size < budget/4 {
					return plan, "segment_too_long"
				}
				continue
			}
			plan.segments, plan.texts = append(plan.segments, parts), append(plan.texts, chunk)
			history, media = history[len(chunk):], rest
			break
		}
		if len(plan.segments) > preloadMaxSegments {
			return plan, "too_many_segments"
		}
	}
	n := len(plan.segments)
	for i, parts := range plan.segments {
		text := parts[0].(object)
		text["text"] = preloadHeader(i+1, n) + text["text"].(string)
	}
	return plan, ""
}

func preloadHeader(i, n int) string {
	return fmt.Sprintf("【较早的对话记录 第 %d/%d 段】调用方的对话太长，超过单条消息上限，所以先分 %d 段发来较早的完整记录（一段 JSON 按顺序切开；system 为调用方指令，tool 为函数执行结果；附件随所在段上传）。最后一段之后会发来其余的新记录，届时请结合全部内容继续。现在只需阅读本段：不要调用任何工具，不要开始执行任务，只回复「收到」。\n", i, n, n)
}

// preloadHistory renders history as a fresh request would (a JSON
// transcript) with each attachment replaced by a unique marker, and returns
// the attachments in order with their markers.
func preloadHistory(history []apiMessage, offset int) (string, []preloadMedia) {
	clone := append([]apiMessage{}, history...)
	media := []preloadMedia{}
	for i, m := range clone {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, part := range parts {
			if field(part, "type") == "text" {
				kept = append(kept, part)
				continue
			}
			marker := fmt.Sprintf("[附件 H%d：原第 %d 条 %s 消息的附件，随所在段上传]", len(media)+1, offset+i+1, m.Role)
			media = append(media, preloadMedia{marker: marker, part: part})
			kept = append(kept, object{"type": "text", "text": marker})
		}
		clone[i].Content = kept
	}
	return plainJSON(clone), media
}

type preloadMedia struct {
	marker string
	part   any
}

// segmentParts is a segment's content: its text, then the attachments whose
// markers it contains. A marker cut in two stays with the later segment.
func segmentParts(chunk string, media []preloadMedia) ([]any, []preloadMedia) {
	parts := []any{object{"type": "text", "text": chunk}}
	n := 0
	for n < len(media) && strings.Contains(chunk, jsonEscaped(media[n].marker)) {
		parts = append(parts, media[n].part)
		n++
	}
	return parts, media[n:]
}

// jsonEscaped is how s appears inside plainJSON output.
func jsonEscaped(s string) string {
	quoted := plainJSON(s)
	return quoted[1 : len(quoted)-1]
}

// cutHistory takes at most size UTF-16 units of s, ending after a JSON
// separator when one is near, and never inside a rune or an escape.
func cutHistory(s string, size int) string {
	if utf16Len(s) <= size {
		return s
	}
	end, units := 0, 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > size {
			break
		}
		units += w
		end = i + utf8.RuneLen(r)
	}
	window := s[:end]
	floor := max(0, end-8192)
	for _, sep := range []string{"},{", "\",\"", ", "} {
		if i := strings.LastIndex(window[floor:], sep); i >= 0 {
			return window[:floor+i+len(sep)-1]
		}
	}
	for end > 0 && s[end-1] == '\\' {
		end--
	}
	return s[:end]
}

// segmentRequest is a synthetic request continuing prefix with one history
// segment, under the caller's model, tools and settings so that the native
// conversation it opens is the one the caller's request continues.
func segmentRequest(req apiRequest, prefix []apiMessage, parts []any) apiRequest {
	s := req
	s.Messages = append(append([]apiMessage{}, prefix...), apiMessage{Role: "user", Content: parts})
	s.ToolChoice, s.ResponseFormat, s.Stream, s.StreamOptions, s.N = nil, nil, false, nil, 0
	s.segment = true
	return s
}

// preload sends the plan's segments and re-files the conversation under the
// caller's history, so that submitting req continues it.
func (g *gateway) preload(ctx context.Context, req apiRequest, plan preloadPlan) string {
	started := time.Now()
	fmt.Fprintf(os.Stderr, "GATEWAY_PRELOAD start segments=%d base=%d split=%d messages=%d\n", len(plan.segments), plan.base, plan.split, len(req.Messages))
	prefix := append([]apiMessage{}, req.Messages[:plan.base]...)
	known := plan.context
	for i, parts := range plan.segments {
		segStart := time.Now()
		s := segmentRequest(req, prefix, parts)
		answer, messages, reason := g.answerSegment(ctx, s)
		if reason != "" {
			return fmt.Sprintf("%s segment=%d", reason, i+1)
		}
		g.mu.Lock()
		th := g.threads[threadKey(threadHeader(s), lastKey(messages, answer))]
		now := 0
		if th != nil {
			now = th.context
		}
		g.mu.Unlock()
		if th == nil {
			return fmt.Sprintf("no_conversation segment=%d", i+1)
		}
		growth, need := now-known, estimateTokens(plan.texts[i])*preloadMinGrowth/100
		fmt.Fprintf(os.Stderr, "GATEWAY_PRELOAD segment=%d/%d context=%d growth=%d need=%d attachments=%d seconds=%.1f\n", i+1, len(plan.segments), now, growth, need, len(parts)-1, time.Since(segStart).Seconds())
		if now == 0 || growth < need {
			// Work dropped earlier context, or did not say: do not continue a
			// conversation that may have lost history.
			g.mu.Lock()
			g.forgetLocked(threadKey(threadHeader(s), lastKey(messages, answer)))
			g.mu.Unlock()
			return fmt.Sprintf("context_not_kept segment=%d context=%d growth=%d need=%d", i+1, now, growth, need)
		}
		known = now
		prefix = append(messages, apiMessage{Role: "assistant", Content: answer})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	header := threadHeader(req)
	from := threadKey(header, threadKeys(prefix)[len(prefix)-1])
	th := g.threads[from]
	if th == nil {
		return "conversation_lost"
	}
	keys := threadKeys(req.Messages[:plan.split])
	to := threadKey(header, keys[len(keys)-1])
	delete(g.threads, from)
	old := g.threads[to]
	th.keys, th.used = keys, time.Now()
	g.threads[to] = th
	if err := g.saveThreadsLocked(); err != nil {
		delete(g.threads, to)
		if old != nil {
			g.threads[to] = old
		}
		g.threads[from] = th
		return "state_persistence_failed"
	}
	if old != nil && old != th {
		g.releaseWorkspaceLocked(old.workspace)
	}
	fmt.Fprintf(os.Stderr, "GATEWAY_PRELOAD done segments=%d context=%d seconds=%.1f\n", len(plan.segments), known, time.Since(started).Seconds())
	return ""
}

func lastKey(messages []apiMessage, answer string) string {
	all := append(append([]apiMessage{}, messages...), apiMessage{Role: "assistant", Content: answer})
	keys := threadKeys(all)
	return keys[len(keys)-1]
}

// answerSegment submits one segment and waits for a plain answer. A model
// that calls a tool anyway is told it cannot, up to preloadToolRounds times.
// It returns the answer and the messages it answered.
func (g *gateway) answerSegment(ctx context.Context, s apiRequest) (string, []apiMessage, string) {
	for round := 0; ; round++ {
		x, failure := g.submit(s, "")
		if failure != nil {
			return "", nil, "submit_" + failure.Code
		}
		select {
		case <-ctx.Done():
			return "", nil, "cancelled"
		case <-x.ready:
		}
		if x.err != nil {
			return "", nil, "upstream_" + x.err.Code
		}
		if len(x.message.ToolCalls) == 0 {
			text, _ := x.message.Content.(string)
			return text, s.Messages, ""
		}
		if round >= preloadToolRounds {
			return "", nil, "tool_calls"
		}
		s.Messages = append(append([]apiMessage{}, s.Messages...), x.message)
		for _, call := range x.message.ToolCalls {
			s.Messages = append(s.Messages, apiMessage{Role: "tool", ToolCallID: call.ID, Content: "现在只是在接收较早的对话记录，不能调用工具。请只回复「收到」。", IsError: true})
		}
	}
}
