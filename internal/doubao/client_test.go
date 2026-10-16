package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tinyReader struct{ data []byte }

func (r *tinyReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
func TestSSEUTF8AndCRLFAtEveryByteBoundary(t *testing.T) {
	raw := "event: TEXT\r\nid: 42\r\ndata: {\"text\":\"你好 🐼\",\r\ndata: \"n\":1}\r\n\r\n"
	seen := 0
	err := consumeSSE(&tinyReader{data: []byte(raw)}, func(name string, v object, id string) (bool, error) {
		seen++
		if name != "TEXT" || id != "42" || textValue(v["text"]) != "你好 🐼" {
			t.Fatal("corrupted SSE frame")
		}
		return false, nil
	})
	if err != nil || seen != 1 {
		t.Fatal(err, seen)
	}
}
func TestTruncatedSSEIsNotDelivered(t *testing.T) {
	seen := 0
	err := consumeSSE(strings.NewReader("event: SSE_REPLY_END\ndata: {\"end_type\":3}\n"), func(string, object, string) (bool, error) { seen++; return false, nil })
	if err == nil || seen != 0 {
		t.Fatal("truncated completion was accepted")
	}
}

func TestUnnamedSSEDeliversNativeToolEvents(t *testing.T) {
	seen := false
	err := consumeSSE(strings.NewReader("id: 1\ndata: {\"event_type\":1,\"tool_call\":{\"id\":\"call\",\"name\":\"connector.call\"}}\n\n"), func(name string, frame object, id string) (bool, error) {
		seen = name == "message" && number(frame["event_type"]) == 1 && id == "1"
		return false, nil
	})
	if err != nil || !seen {
		t.Fatal("native SSE event was dropped", err)
	}
}
func TestCookieScopeAndExpirySurviveRestart(t *testing.T) {
	dir := t.TempDir()
	c, err := newClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(origin + "/passport/web/get_qrcode/")
	c.jar.SetCookies(u, []*http.Cookie{{Name: "auth", Value: "synthetic-secret", Secure: true, Path: "/passport", MaxAge: 3600}})
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := newClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	chat, _ := url.Parse(origin + "/chat/completion")
	if len(loaded.jar.Cookies(chat)) != 0 || len(loaded.jar.Cookies(u)) != 1 {
		t.Fatal("cookie scope changed on restart")
	}
	loaded.jar.SetCookies(u, []*http.Cookie{{Name: "auth", Path: "/passport", MaxAge: -1}})
	if err := loaded.save(); err != nil {
		t.Fatal(err)
	}
	again, err := newClient(dir)
	if err != nil || len(again.jar.Cookies(u)) != 0 {
		t.Fatal("expired cookie resurrected")
	}
}
func TestEndpointGuardRejectsCredentialExfiltration(t *testing.T) {
	for _, s := range []string{"https://doubao.com.attacker.invalid/", "http://www.doubao.com/", "https://user@www.doubao.com/", "https://www.doubao.com:443/"} {
		if _, err := trustedURL(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAcknowledgedDisconnectNeverResubmitsOrClaimsSuccess(t *testing.T) {
	dir := t.TempDir()
	c, err := newClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	sends := 0
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/chat/completion" {
			sends++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"12345678901234567\"},\"query_list\":[{\"question_id\":\"12345678901234568\"}]}\n\n"))}, nil
		}
		return nil, errors.New("synthetic disconnect")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := c.run(ctx, buildBody("fixture", "", "chat", Model{ID: "1", Think: 1}), filepath.Join(dir, "receipt.json"))
	if err == nil || !r.Accepted || r.Completed || sends != 1 {
		t.Fatalf("unsafe recovery: sends=%d accepted=%v completed=%v err=%v", sends, r.Accepted, r.Completed, err)
	}
}
func TestFinalAnswerExcludesThinkingDescendants(t *testing.T) {
	m := object{"content_block": []any{object{"block_id": "hidden-root", "content": object{"thinking_block": object{}}}, object{"block_id": "nested", "parent_id": "hidden-root", "content": object{"text_block": object{"text": "private reasoning"}}}, object{"block_id": "answer", "content": object{"text_block": object{"text": "VISIBLE"}}}}}
	if visibleText(m) != "VISIBLE" {
		t.Fatal("thinking leaked into answer")
	}
}

func TestPassportAbsoluteExpiryAndTTL(t *testing.T) {
	now := time.Unix(1792062648, 0)
	for _, raw := range []any{1792062708, int64(1792062708000), 60} {
		deadline, seconds := qrExpiry(raw, now)
		if seconds != 60 || !deadline.Equal(now.Add(time.Minute)) {
			t.Fatalf("expiry metadata wrong for %v: %v, %d", raw, deadline, seconds)
		}
	}
}

func TestWorkOnlyCatalogAndExplicitModeMismatch(t *testing.T) {
	models := []Model{{Name: "Disabled", Available: false, Modes: []string{"chat"}}, {Name: "Work model", Available: true, Modes: []string{"work"}}}
	m, mode, err := selectModel(models, "Work model", "auto")
	if err != nil || m.Name != "Work model" || mode != "work" {
		t.Fatal("available Work model not selected", err)
	}
	if _, _, err := selectModel(models, "Work model", "chat"); err == nil {
		t.Fatal("explicit incompatible mode was ignored")
	}
}

func TestIMReadbackUsesServiceEncodingParameter(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/im/conversation/batch_get" || req.Header.Get("Content-Type") != "application/json; encoding=utf-8" {
			t.Fatal("IM request lacks its required encoding parameter")
		}
		body := `{"status_code":0,"downlink_body":{"batch_get_conv_info_downlink_body":{"conversation_info_list":[{"conversation_id":"12345678901234567","messages":[{"user_type":1,"message_id":"12345678901234568"},{"user_type":2,"message_id":"12345678901234569","bot_reply_message_id":"12345678901234568","ext":{"is_finish":"1"},"content_block":[{"block_id":"answer","content":{"text_block":{"text":"POC_OK"}}}]}]}]}}}`
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	answer, done, err := c.readReply(context.Background(), &Receipt{Accepted: true, ConversationID: "12345678901234567", RunID: "12345678901234568"})
	if err != nil || !done || answer != "POC_OK" {
		t.Fatal("readback failed", answer, done, err)
	}
}
