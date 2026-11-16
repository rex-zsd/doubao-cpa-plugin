package doubao

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

type preloadTurn struct {
	conversation string
	continued    int
	segment      bool
	chars        int
	prompt       string
	attachments  int
}

// preloadRunner plays Work: each conversation's context grows by the
// estimated tokens of every prompt, unless keep returns false, when Work
// "truncates" and reports the context of the last prompt only.
func preloadRunner(g *gateway, keep func(n int) bool) (func(*gatewayTurn), func() []preloadTurn) {
	var mu sync.Mutex
	turns := []preloadTurn{}
	contexts := map[string]int{}
	n := 0
	run := func(turn *gatewayTurn) {
		g.mu.Lock()
		prompt := outgoingPrompt(turn.request, turn.continued)
		seen := preloadTurn{conversation: turn.conversation, continued: turn.continued, segment: turn.request.segment, chars: utf16Len(prompt), prompt: prompt}
		last := turn.request.Messages[len(turn.request.Messages)-1]
		if parts, ok := last.Content.([]any); ok {
			for _, part := range parts {
				if field(part, "type") != "text" {
					seen.attachments++
				}
			}
		}
		if turn.conversation == "" {
			n++
			turn.conversation = fmt.Sprintf("conv-%d", n)
		}
		turn.mode = "work"
		mu.Lock()
		turns = append(turns, seen)
		k := len(turns)
		if keep(k) {
			contexts[turn.conversation] += estimateTokens(prompt)
		} else {
			contexts[turn.conversation] = estimateTokens(prompt)
		}
		g.noteContextLocked(turn, contexts[turn.conversation])
		mu.Unlock()
		g.mu.Unlock()
		answer := "收到"
		if !turn.request.segment {
			answer = "final answer"
		}
		g.finish(turn, answer, nil)
	}
	return run, func() []preloadTurn {
		mu.Lock()
		defer mu.Unlock()
		return append([]preloadTurn{}, turns...)
	}
}

// longHistory is a conversation of n answered turns of size characters each,
// then a short question.
func longHistory(n, size int) apiRequest {
	texts := []string{}
	for i := 0; i < n; i++ {
		texts = append(texts, fmt.Sprintf("marker-%d ", i)+strings.Repeat("x", size), fmt.Sprintf("answer %d", i))
	}
	return chatRequest(append(texts, "which markers did you see?")...)
}

func TestLongHistoryIsPreloadedIntoTheAnsweringConversation(t *testing.T) {
	g, server := testGateway(t)
	run, turns := preloadRunner(g, func(int) bool { return true })
	g.run = run
	req := longHistory(4, 200000)
	if utf16Len(promptText(req)) <= maxPromptChars {
		t.Fatal("fixture fits one message")
	}
	status, response := postGateway(t, server, req, "")
	if status != 200 || textValue(returnedMessage(t, response).Content) != "final answer" {
		t.Fatal(status, response)
	}
	seen := turns()
	if len(seen) < 3 {
		t.Fatalf("%d turns", len(seen))
	}
	all := ""
	for i, turn := range seen {
		if turn.chars > maxPromptChars {
			t.Fatalf("turn %d renders %d characters", i+1, turn.chars)
		}
		last := i == len(seen)-1
		if turn.segment == last {
			t.Fatalf("turn %d segment=%t", i+1, turn.segment)
		}
		if i > 0 && turn.conversation != "conv-1" {
			t.Fatalf("turn %d ran in %q", i+1, turn.conversation)
		}
		if !last && !strings.Contains(turn.prompt, fmt.Sprintf("第 %d/%d 段", i+1, len(seen)-1)) {
			t.Fatalf("segment %d header missing", i+1)
		}
		all += turn.prompt
	}
	for i := 0; i < 4; i++ {
		if !strings.Contains(all, fmt.Sprintf("marker-%d ", i)) {
			t.Fatalf("marker %d never sent", i)
		}
	}
	final := seen[len(seen)-1]
	if final.continued == 0 || !strings.Contains(final.prompt, "which markers") || strings.Contains(final.prompt, "marker-0") {
		t.Fatalf("final turn continued=%d", final.continued)
	}
	// The conversation keeps serving the caller's history afterwards.
	next := req
	next.Messages = append(append([]apiMessage{}, req.Messages...), apiMessage{Role: "assistant", Content: "final answer"}, apiMessage{Role: "user", Content: "and now?"})
	if status, response = postGateway(t, server, next, ""); status != 200 {
		t.Fatal(status, response)
	}
	seen = turns()
	if last := seen[len(seen)-1]; last.conversation != "conv-1" || last.continued != len(req.Messages)+1 || last.segment {
		t.Fatalf("follow-up %+v", last)
	}
}

func TestPreloadStopsWhenWorkDropsHistory(t *testing.T) {
	g, server := testGateway(t)
	run, turns := preloadRunner(g, func(k int) bool { return k != 2 })
	g.run = run
	status, response := postGateway(t, server, longHistory(4, 200000), "")
	if status != 400 || field(response, "error", "code") != "context_length_exceeded" {
		t.Fatal(status, response)
	}
	seen := turns()
	if len(seen) != 2 {
		t.Fatalf("%d turns after the drop", len(seen))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.threads) != 0 {
		t.Fatalf("%d threads kept", len(g.threads))
	}
}

func TestPreloadNeedsATailThatFits(t *testing.T) {
	g, server := testGateway(t)
	run, turns := preloadRunner(g, func(int) bool { return true })
	g.run = run
	req := chatRequest("a", "b", strings.Repeat("y", maxPromptChars+10))
	status, response := postGateway(t, server, req, "")
	if status != 400 || field(response, "error", "code") != "context_length_exceeded" || len(turns()) != 0 {
		t.Fatal(status, response, len(turns()))
	}
}

func TestPreloadCarriesAttachmentsWithTheirSegment(t *testing.T) {
	g, server := testGateway(t)
	run, turns := preloadRunner(g, func(int) bool { return true })
	g.run = run
	req := longHistory(3, 200000)
	image := object{"type": "image_url", "image_url": object{"url": "data:image/png;base64,YQ=="}}
	req.Messages[0].Content = []any{object{"type": "text", "text": textValue(req.Messages[0].Content)}, image}
	status, response := postGateway(t, server, req, "")
	if status != 200 {
		t.Fatal(status, response)
	}
	carried := 0
	for _, turn := range turns() {
		if turn.attachments > 0 {
			carried += turn.attachments
			if !turn.segment || !strings.Contains(turn.prompt, "附件 H1") {
				t.Fatalf("attachment arrived with %+v", turn.segment)
			}
		}
	}
	if carried != 1 {
		t.Fatalf("%d attachments carried", carried)
	}
}

func TestCutHistoryKeepsRunesAndEscapes(t *testing.T) {
	s := strings.Repeat("中", 10) + `\"` + strings.Repeat("𝄞", 5)
	for size := 1; size < 30; size++ {
		cut := cutHistory(s, size)
		if utf16Len(cut) > size || !strings.HasPrefix(s, cut) || strings.HasSuffix(cut, `\`) {
			t.Fatalf("size %d cut %q", size, cut)
		}
	}
}
