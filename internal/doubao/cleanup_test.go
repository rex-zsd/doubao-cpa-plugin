package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Only conversations this client created are recorded; continuing one does
// not add another record.
func TestConversationLedgerRecordsCreatedConversationsOnly(t *testing.T) {
	u := &resumeUpstream{completion: resumeBody(resumeACK, false)}
	c := u.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.runObserved(ctx, buildBody("fixture", "", "work", Model{ID: "1", Think: 1}), c.dir+"/receipt.json", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.runObserved(ctx, buildBody("again", resumeConversation, "work", Model{ID: "1", Think: 1}), c.dir+"/receipt2.json", nil); err != nil {
		t.Fatal(err)
	}
	l := c.loadLedgerLocked()
	if len(l.Created) != 1 || l.Created[resumeConversation].IsZero() {
		t.Fatal("ledger", l.Created)
	}
}

func ledgerGateway(t *testing.T, ages map[string]time.Duration) (*gateway, *Service, *[][]string, *bool) {
	t.Helper()
	g, _ := testGateway(t)
	l := conversationLedger{Created: map[string]time.Time{}}
	for id, age := range ages {
		l.Created[id] = time.Now().Add(-age)
	}
	if err := saveJSON(g.c.ledgerPath(), l); err != nil {
		t.Fatal(err)
	}
	batches, refuse := [][]string{}, false
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/im/conversation/batch_del_user_conv" {
			return nil, errors.New("unexpected " + req.URL.Path)
		}
		var body struct {
			Cmd    int `json:"cmd"`
			Uplink struct {
				Delete struct {
					IDs  []string `json:"conversation_id"`
					All  bool     `json:"delete_all"`
					Type int      `json:"conversation_type"`
				} `json:"batch_delete_user_conversation_uplink_body"`
			} `json:"uplink_body"`
		}
		if err := decode(req.Body, &body); err != nil || body.Cmd != 4171 || body.Uplink.Delete.All || body.Uplink.Delete.Type != 3 {
			t.Error("unexpected delete request; delete_all must stay false", err, body)
		}
		batches = append(batches, body.Uplink.Delete.IDs)
		answer := `{"status_code":0}`
		if refuse {
			answer = `{"status_code":7}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(answer))}, nil
	})
	return g, &Service{c: g.c, g: g}, &batches, &refuse
}

// Deletion takes recorded conversations past the age, never one a running
// task or a continuable thread uses, and forgets those Work deleted.
func TestDeleteConversationsSkipsRecentAndInUse(t *testing.T) {
	g, s, batches, _ := ledgerGateway(t, map[string]time.Duration{"old": 10 * 24 * time.Hour, "threaded": 10 * 24 * time.Hour, "recent": time.Hour})
	g.mu.Lock()
	g.threads["k"] = &gatewayThread{conversation: "threaded", used: time.Now()}
	g.mu.Unlock()
	if sum := s.Conversations(); sum.Total != 3 || sum.InUse != 1 || sum.Older["7"] != 1 || sum.Older["0"] != 2 {
		t.Fatalf("summary %+v", sum)
	}
	deleted, remaining, err := s.DeleteConversations(context.Background(), 7*24*time.Hour)
	if err != nil || deleted != 1 || remaining != 0 || len(*batches) != 1 || len((*batches)[0]) != 1 || (*batches)[0][0] != "old" {
		t.Fatal("deleted the wrong conversations", deleted, remaining, err, *batches)
	}
	l := g.c.loadLedgerLocked()
	if _, ok := l.Created["old"]; ok || len(l.Created) != 2 {
		t.Fatal("deleted conversation not forgotten", l.Created)
	}
}

// A refused deletion keeps the records, so nothing is lost track of.
func TestRefusedDeletionKeepsRecords(t *testing.T) {
	g, s, _, refuse := ledgerGateway(t, map[string]time.Duration{"a": 2 * time.Hour, "b": 2 * time.Hour})
	*refuse = true
	deleted, remaining, err := s.DeleteConversations(context.Background(), 0)
	if err == nil || deleted != 0 || remaining != 2 {
		t.Fatal("refusal not reported", deleted, remaining, err)
	}
	if l := g.c.loadLedgerLocked(); len(l.Created) != 2 {
		t.Fatal("records dropped after a refused deletion", l.Created)
	}
}

// Connector cleanup only disconnects plugin-named connectors that no task
// holds, never the user's own, and not while a task runs.
func TestConnectorCleanupTouchesOnlyIdlePluginConnectors(t *testing.T) {
	g, server := testGateway(t)
	s := &Service{c: g.c, g: g}
	disconnected := []string{}
	g.c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"items":[{"connector_id":"plugin-left","name":"independent-http-tools-0123456789ab","enabled":true},{"connector_id":"plugin-held","name":"independent-http-tools-ba9876543210","enabled":true},{"connector_id":"user-own","name":"my-calendar","enabled":true}],"has_more":false}}`
		if strings.HasSuffix(req.URL.Path, "/connector/disconnect") {
			var v object
			_ = decode(req.Body, &v)
			disconnected = append(disconnected, textValue(v["connector_id"]))
			body = `{"code":0}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	release := make(chan struct{})
	g.run = func(turn *gatewayTurn) {
		g.mu.Lock()
		turn.connector = "plugin-held"
		g.mu.Unlock()
		<-release
		g.finish(turn, "done", nil)
	}
	go func() {
		// The response arrives only once the task ends; nothing to check here.
		if r, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(jsonText(toolRequest()))); err == nil {
			r.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.connectorUse(); st.inUse["plugin-held"] {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sum, err := s.Connectors(context.Background())
	if err != nil || sum.Total != 2 || sum.InUse != 1 || sum.Leftover != 1 {
		t.Fatalf("summary %+v %v", sum, err)
	}
	if _, err := s.CleanupConnectors(context.Background()); err == nil {
		t.Fatal("cleanup ran while a task was running")
	}
	close(release)
	g.wg.Wait()
	g.mu.Lock()
	for _, turn := range g.turns {
		turn.connector = ""
	}
	g.mu.Unlock()
	removed, err := s.CleanupConnectors(context.Background())
	if err != nil || removed != 2 || len(disconnected) != 2 {
		t.Fatal("idle plugin connectors not cleaned", removed, err, disconnected)
	}
	for _, id := range disconnected {
		if id == "user-own" {
			t.Fatal("disconnected the user's own connector")
		}
	}
}
