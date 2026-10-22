package doubao

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type threadStart struct {
	conversation string
	continued    int
}

// threadRunner answers every task with its conversation and last message, so
// tests can follow which conversation each request landed in.
func threadRunner(g *gateway, starts chan<- threadStart) func(*gatewayTurn) {
	n := 0
	return func(turn *gatewayTurn) {
		g.mu.Lock()
		start := threadStart{turn.conversation, turn.continued}
		if turn.conversation == "" {
			n++
			turn.conversation = fmt.Sprintf("conv-%d", n)
		}
		if turn.mode == "" {
			turn.mode = "chat"
		}
		turn.generationPosts++
		answer := turn.conversation + ": " + textValue(turn.request.Messages[len(turn.request.Messages)-1].Content)
		g.mu.Unlock()
		starts <- start
		g.finish(turn, answer, nil)
	}
}

func chatRequest(texts ...string) apiRequest {
	req := apiRequest{Model: "TestModel"}
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		req.Messages = append(req.Messages, apiMessage{Role: role, Content: text})
	}
	return req
}

func TestThreadContinuesOnlyTheExactlyExtendedHistory(t *testing.T) {
	g, server := testGateway(t)
	starts := make(chan threadStart, 16)
	g.run = threadRunner(g, starts)
	ask := func(req apiRequest, want threadStart) string {
		t.Helper()
		status, response := postGateway(t, server, req, "")
		if status != 200 {
			t.Fatal(status, response)
		}
		if got := <-starts; got != want {
			t.Fatalf("started %+v, want %+v", got, want)
		}
		return textValue(returnedMessage(t, response).Content)
	}
	first := ask(chatRequest("a"), threadStart{"", 0})
	second := ask(chatRequest("a", first, "b"), threadStart{"conv-1", 2})
	// Regenerating the same turn cannot reuse a conversation that already
	// holds an answer to it.
	if again := ask(chatRequest("a", first, "b"), threadStart{"", 0}); again == second {
		t.Fatal("regenerated answer came from the same conversation")
	}
	// Caller formatting of the earlier answer does not matter.
	req := chatRequest("a", first, "b", "", "c")
	req.Messages[3].Content = []any{object{"type": "text", "text": "\n" + second + "  "}}
	ask(req, threadStart{"conv-1", 4})
	// An edited earlier message opens a new conversation.
	ask(chatRequest("a", first+" (edited)", "b"), threadStart{"", 0})
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.threads) != 3 {
		t.Fatal("unexpected threads", len(g.threads))
	}
}

func TestThreadKeysCoverHistoryModelAndTools(t *testing.T) {
	base := chatRequest("a", "A", "b")
	key := func(req apiRequest) string {
		keys := threadKeys(req.Messages)
		return threadKey(threadHeader(req), keys[len(keys)-1])
	}
	model := base
	model.Model = "OtherModel"
	tools := toolRequest()
	tools.Messages = base.Messages
	otherTools := tools
	otherTools.Tools = append([]apiTool{}, tools.Tools...)
	otherTools.Tools[0].Function.Description = "changed"
	choice := tools
	choice.ToolChoice = "required"
	unused := tools
	unused.ToolChoice = "none"
	spaced := chatRequest(" a ", "A\n", "b")
	called := chatRequest("a", "A", "b")
	called.Messages[1].ToolCalls = []apiToolCall{{ID: "call", Type: "function", Function: apiCallFunction{Name: "f", Arguments: "{}"}}}
	if key(model) == key(base) || key(tools) == key(base) || key(otherTools) == key(tools) || key(called) == key(base) {
		t.Fatal("different histories share a thread key")
	}
	if key(choice) != key(tools) || key(unused) != key(base) || key(spaced) != key(base) {
		t.Fatal("equivalent histories have different thread keys")
	}
}

func TestExpiredThreadsAndTheirFoldersAreDropped(t *testing.T) {
	g, server := testGateway(t)
	starts := make(chan threadStart, 4)
	g.run = threadRunner(g, starts)
	status, response := postGateway(t, server, chatRequest("a"), "")
	if status != 200 {
		t.Fatal(status, response)
	}
	<-starts
	first := textValue(returnedMessage(t, response).Content)
	folder := filepath.Join(g.dir, "gateway-workspace-thread")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	for _, th := range g.threads {
		th.used, th.workspace = time.Now().Add(-threadTTL-time.Minute), folder
	}
	g.mu.Unlock()
	if status, response = postGateway(t, server, chatRequest("a", first, "b"), ""); status != 200 {
		t.Fatal(status, response)
	}
	if got := <-starts; got.conversation != "" {
		t.Fatal("expired thread was continued", got)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatal("expired thread kept its folder", err)
	}
}

func TestTaskFoldersLiveUntilNoTaskOrThreadUsesThem(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	turn := persistedTurn(t, g, true)
	folder := filepath.Join(g.dir, "gateway-workspace-"+turn.id)
	orphan := filepath.Join(g.dir, "gateway-workspace-orphan")
	for _, path := range []string{folder, orphan} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// After a restart only recovered tasks hold folders.
	restored := newGateway(context.Background(), c, nil, c.dir, time.Minute)
	if err := restored.recoverTasks(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan folder survived recovery", err)
	}
	restored.mu.Lock()
	defer restored.mu.Unlock()
	turn = restored.turns[turn.id]
	turn.workspace = folder
	restored.threads["key"] = &gatewayThread{workspace: folder, used: time.Now()}
	restored.dropTurnLocked(turn)
	if _, err := os.Stat(folder); err != nil {
		t.Fatal("folder removed while a thread used it", err)
	}
	restored.forgetLocked("key")
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatal("unused folder was kept", err)
	}
}

// fakeChat serves Work's generation and readback endpoints. reject decides,
// from a generation body, whether Work refuses it before accepting it.
type fakeChat struct {
	mu     sync.Mutex
	bodies []object
	runs   map[string]string
	n      int
	reject func(object) int
}

func (f *fakeChat) RoundTrip(r *http.Request) (*http.Response, error) {
	var body object
	if err := decode(r.Body, &body); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(kind, text string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(text))}, nil
	}
	switch r.URL.Path {
	case "/chat/completion":
		f.bodies = append(f.bodies, body)
		if f.reject != nil {
			if code := f.reject(body); code != 0 {
				return reply("text/event-stream", "event: STREAM_ERROR\ndata: "+jsonText(object{"error_code": code, "error_msg": "refused"})+"\n\n")
			}
		}
		f.n++
		conversation := textValue(field(body, "client_meta", "conversation_id"))
		if conversation == "" {
			conversation = fakeConversation(f.n)
		}
		run := fmt.Sprint(2000000000000 + f.n)
		if f.runs == nil {
			f.runs = map[string]string{}
		}
		f.runs[conversation] = run
		return reply("text/event-stream", "event: SSE_ACK\ndata: "+jsonText(object{"ack_client_meta": object{"conversation_id": conversation}, "query_list": []any{object{"question_id": run}}})+"\n\nevent: SSE_REPLY_END\ndata: {\"end_type\":3}\n\n")
	case "/im/conversation/batch_get":
		conversation := textValue(array(field(body, "uplink_body", "batch_get_conv_info_uplink_body", "conversation_id"))[0])
		run := f.runs[conversation]
		messages := []any{object{"user_type": 1, "message_id": run}, object{"user_type": 2, "message_id": run + "9", "bot_reply_message_id": run, "status": 0, "ext": object{"is_finish": "1"}, "content_block": []any{object{"content": object{"text_block": object{"text": "reply " + run}}}}}}
		return reply("application/json", jsonText(object{"status_code": 0, "downlink_body": object{"batch_get_conv_info_downlink_body": object{"conversation_info_list": []any{object{"conversation_id": conversation, "messages": messages}}}}}))
	}
	return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
}

func fakeConversation(n int) string { return fmt.Sprint(1000000000000 + n) }

func (f *fakeChat) last() object {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func sentText(body object) string {
	blocks := array(field(array(body["messages"])[0], "content_block"))
	return textValue(field(blocks[0], "content", "text_block", "text"))
}

func TestPlainRequestsUseChatModeAndContinueTheirConversation(t *testing.T) {
	g, server := testGateway(t)
	chat := &fakeChat{}
	g.c.http.Transport = chat
	ask := func(req apiRequest) string {
		t.Helper()
		status, response := postGateway(t, server, req, "")
		if status != 200 {
			t.Fatal(status, response)
		}
		return textValue(returnedMessage(t, response).Content)
	}
	first := ask(chatRequest("a"))
	body := chat.last()
	if number(field(body, "option", "agent_mode")) != 2 || field(body, "option", "conversation_init_ext", "mode_id") != "1" || field(body, "option", "need_create_conversation") != true || sentText(body) != "a" {
		t.Fatal("first request was not a new chat-mode conversation", jsonText(field(body, "option")))
	}
	conversation := textValue(field(body, "client_meta", "conversation_id"))
	if conversation != "" {
		t.Fatal("new conversation named an id", conversation)
	}
	second := ask(chatRequest("a", first, "b"))
	body = chat.last()
	if field(body, "client_meta", "conversation_id") != fakeConversation(1) || field(body, "option", "need_create_conversation") != false || field(body, "option", "conversation_init_ext") != nil || number(field(body, "option", "agent_mode")) != 2 || sentText(body) != "b" {
		t.Fatal("follow-up did not continue the conversation with only the new message", jsonText(body["client_meta"]), sentText(body))
	}
	// Work refusing the continuation before accepting it resends the whole
	// transcript as a new conversation.
	chat.reject = func(body object) int {
		if textValue(field(body, "client_meta", "conversation_id")) != "" {
			return 710000001
		}
		return 0
	}
	ask(chatRequest("a", first, "b", second, "c"))
	body = chat.last()
	if field(body, "client_meta", "conversation_id") != "" || !strings.Contains(sentText(body), "完整对话记录") || !strings.Contains(sentText(body), `"c"`) || number(field(body, "option", "agent_mode")) != 2 {
		t.Fatal("refused continuation was not resent in full", sentText(body))
	}
	// A request chat mode refuses goes to Work.
	chat.reject = func(body object) int {
		if number(field(body, "option", "agent_mode")) == 2 {
			return 710000002
		}
		return 0
	}
	work := ask(chatRequest("x"))
	body = chat.last()
	if number(field(body, "option", "agent_mode")) != 1 || field(body, "option", "conversation_init_ext", "mode_id") != "3" {
		t.Fatal("chat-mode refusal did not fall back to Work", jsonText(field(body, "option")))
	}
	// A Work-mode conversation continues in Work mode.
	chat.reject = nil
	before := len(chat.bodies)
	ask(chatRequest("x", work, "y"))
	body = chat.last()
	if len(chat.bodies) != before+1 || number(field(body, "option", "agent_mode")) != 1 || field(body, "client_meta", "conversation_id") == "" || sentText(body) != "y" {
		t.Fatal("Work-mode follow-up did not continue in Work mode", len(chat.bodies)-before, jsonText(body["client_meta"]))
	}
	// An oversized prompt is not resent to Work.
	chat.reject = func(object) int { return 710020202 }
	before = len(chat.bodies)
	status, response := postGateway(t, server, chatRequest("z"), "")
	if status != 400 || field(response, "error", "code") != "context_length_exceeded" || len(chat.bodies) != before+1 {
		t.Fatal("prompt-too-long was retried or misreported", status, response, len(chat.bodies)-before)
	}
}

func TestContinuedPromptsAndBodies(t *testing.T) {
	req := toolRequest()
	req.Messages = []apiMessage{{Role: "user", Content: "b"}, {Role: "user", Content: "c"}}
	prompt := renderAPIPrompt(req, connectorName("0123456789abcdef"), true)
	if !strings.Contains(prompt, "上一条回答之后新增的对话记录") || !strings.Contains(prompt, "调用方本轮可用的外部函数") || strings.Contains(prompt, "之前轮次的工具名已失效") || strings.Contains(prompt, "完整对话记录") {
		t.Fatal(prompt)
	}
	body := localToolBody("fixture", "1000000000001", Model{ID: "1", Think: 1}, "/owned", "sandbox", "123", "456", []any{})
	if field(body, "client_meta", "conversation_id") != "1000000000001" || field(body, "option", "need_create_conversation") != false || field(body, "option", "conversation_init_ext") != nil || number(field(body, "option", "general_task_param", "runtime_type")) != 2 {
		t.Fatal("continued local-runtime body", jsonText(body["option"]))
	}
}
