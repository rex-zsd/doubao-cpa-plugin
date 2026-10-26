package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The plugin page can remove what the plugin leaves in the Doubao account.
// It removes only what it can prove the plugin created: conversations this
// client recorded when Work acknowledged them, and connectors carrying the
// plugin's connector name prefix. Work's conversation list shows plugin
// conversations exactly like the user's own, so earlier ones stay untouched.

const ledgerLimit = 20000

type conversationLedger struct {
	Created map[string]time.Time `json:"created"`
}

func (c *Client) ledgerPath() string { return filepath.Join(c.dir, "plugin-conversations.json") }

func (c *Client) loadLedgerLocked() conversationLedger {
	l := conversationLedger{Created: map[string]time.Time{}}
	if b, err := os.ReadFile(c.ledgerPath()); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	if l.Created == nil {
		l.Created = map[string]time.Time{}
	}
	return l
}

// noteConversation records a conversation this client created. A failed write
// leaves it unrecorded, so it is never deleted later: the safe direction.
func (c *Client) noteConversation(id string) {
	c.ledgerMu.Lock()
	defer c.ledgerMu.Unlock()
	l := c.loadLedgerLocked()
	if _, ok := l.Created[id]; ok {
		return
	}
	l.Created[id] = time.Now().UTC()
	for len(l.Created) > ledgerLimit {
		oldest := ""
		for id, at := range l.Created {
			if oldest == "" || at.Before(l.Created[oldest]) {
				oldest = id
			}
		}
		delete(l.Created, oldest)
	}
	_ = saveJSON(c.ledgerPath(), l)
}

// ConversationSummary counts recorded conversations by age.
type ConversationSummary struct {
	Total  int            `json:"total"`
	InUse  int            `json:"in_use"`
	Oldest time.Time      `json:"oldest,omitzero"`
	Older  map[string]int `json:"older_than_days"`
}

// conversationsInUse are those a running task or a continuable thread still uses.
func (g *gateway) conversationsInUse() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	used := map[string]bool{}
	for _, t := range g.turns {
		if !t.done && t.conversation != "" {
			used[t.conversation] = true
		}
	}
	for _, th := range g.threads {
		used[th.conversation] = true
	}
	return used
}

func (s *Service) Conversations() ConversationSummary {
	s.c.ledgerMu.Lock()
	l := s.c.loadLedgerLocked()
	s.c.ledgerMu.Unlock()
	used := s.g.conversationsInUse()
	summary := ConversationSummary{Total: len(l.Created), Older: map[string]int{}}
	for id, at := range l.Created {
		if used[id] {
			summary.InUse++
			continue
		}
		if summary.Oldest.IsZero() || at.Before(summary.Oldest) {
			summary.Oldest = at
		}
		for _, days := range []int{0, 1, 3, 7, 30} {
			if time.Since(at) >= time.Duration(days)*24*time.Hour {
				summary.Older[itoaDays(days)]++
			}
		}
	}
	return summary
}

func itoaDays(days int) string {
	b, _ := json.Marshal(days)
	return string(b)
}

// DeleteConversations deletes recorded conversations older than the given age
// that no task or thread uses, 20 per request, and forgets each one Work
// confirmed deleting.
func (s *Service) DeleteConversations(ctx context.Context, olderThan time.Duration) (deleted, remaining int, err error) {
	s.c.ledgerMu.Lock()
	l := s.c.loadLedgerLocked()
	s.c.ledgerMu.Unlock()
	used := s.g.conversationsInUse()
	ids := []string{}
	for id, at := range l.Created {
		if !used[id] && time.Since(at) >= olderThan {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += 20 {
		batch := ids[start:min(start+20, len(ids))]
		// The web client deletes through IM command 4171, one-to-bot chats being
		// conversation type 3; delete_all stays false.
		v, err := s.c.json(ctx, "POST", "/im/conversation/batch_del_user_conv", nil, object{"cmd": 4171, "uplink_body": object{"batch_delete_user_conversation_uplink_body": object{"conversation_id": batch, "delete_all": false, "conversation_type": 3}}, "sequence_id": token(), "channel": 2, "version": "1"})
		if err == nil && number(v["status_code"]) != 0 {
			err = fmt.Errorf("Work refused to delete conversations, status_code=%d", number(v["status_code"]))
		}
		if err != nil {
			return deleted, len(ids) - deleted, err
		}
		s.c.ledgerMu.Lock()
		current := s.c.loadLedgerLocked()
		for _, id := range batch {
			delete(current.Created, id)
		}
		saveErr := saveJSON(s.c.ledgerPath(), current)
		s.c.ledgerMu.Unlock()
		if saveErr != nil {
			return deleted + len(batch), len(ids) - deleted - len(batch), saveErr
		}
		deleted += len(batch)
	}
	return deleted, 0, nil
}

// connectorPrefix starts the name of every connector the plugin creates.
const connectorPrefix = "independent-http-tools-"

// ConnectorSummary classifies the plugin's connectors in the account.
type ConnectorSummary struct {
	Total    int `json:"total"`
	InUse    int `json:"in_use"`
	Cached   int `json:"cached"`
	Leftover int `json:"leftover"`
}

type connectorState struct {
	inUse, cached map[string]bool
	idle          bool
	starts        int
}

// connectorUse reads which connector IDs tasks hold and which the cache
// keeps idle. The pool lock is never taken while the gateway lock is held.
func (s *Service) connectorUse() connectorState {
	state := connectorState{inUse: map[string]bool{}, cached: map[string]bool{}}
	if p := s.g.connectors; p != nil {
		p.mu.Lock()
		for _, e := range p.entries {
			if e.ID == "" {
				continue
			}
			if len(e.Holders) > 0 || e.Phase != "ready" {
				state.inUse[e.ID] = true
			} else {
				state.cached[e.ID] = true
			}
		}
		p.mu.Unlock()
	}
	g := s.g
	g.mu.Lock()
	state.idle, state.starts = true, g.taskStarts
	for _, t := range g.turns {
		if !t.done {
			state.idle = false
			if t.connector != "" {
				state.inUse[t.connector] = true
			}
		}
	}
	g.mu.Unlock()
	return state
}

func (s *Service) pluginConnectors(ctx context.Context) ([]string, error) {
	rows, err := s.c.connectorList(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, row := range rows {
		if strings.HasPrefix(textValue(field(row, "name")), connectorPrefix) {
			ids = append(ids, textValue(field(row, "connector_id")))
		}
	}
	return ids, nil
}

func (s *Service) Connectors(ctx context.Context) (ConnectorSummary, error) {
	ids, err := s.pluginConnectors(ctx)
	if err != nil {
		return ConnectorSummary{}, err
	}
	state := s.connectorUse()
	summary := ConnectorSummary{Total: len(ids)}
	for _, id := range ids {
		switch {
		case state.inUse[id]:
			summary.InUse++
		case state.cached[id]:
			summary.Cached++
		default:
			summary.Leftover++
		}
	}
	return summary, nil
}

// CleanupConnectors retires idle cached connectors through the cache and
// disconnects leftover plugin connectors. It runs only while no task runs,
// and stops if one starts meanwhile, since a starting task may own a new
// connector before recording it.
func (s *Service) CleanupConnectors(ctx context.Context) (removed int, err error) {
	before := s.connectorUse()
	if !before.idle {
		return 0, apiFail(409, "tasks_running", "Wait until no task is running before cleaning up connectors")
	}
	if p := s.g.connectors; p != nil {
		if err := p.reap(ctx, true); err != nil {
			return 0, err
		}
	}
	ids, err := s.pluginConnectors(ctx)
	if err != nil {
		return 0, err
	}
	after := s.connectorUse()
	if !after.idle || after.starts != before.starts {
		return 0, apiFail(409, "tasks_running", "A task started during the cleanup; try again when it has finished")
	}
	for _, id := range ids {
		if after.inUse[id] || after.cached[id] {
			continue
		}
		if _, err := s.c.office(ctx, "/alice/office/skills/manage/connector/disconnect", object{"connector_id": id, "skill_type": 1}); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// FileSummary counts files uploaded through the plugin's file API.
type FileSummary struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

func (s *Service) Files() (FileSummary, error) {
	files, err := s.c.listFiles()
	if err != nil {
		return FileSummary{}, err
	}
	summary := FileSummary{Count: len(files)}
	for _, f := range files {
		summary.Bytes += f.Bytes
	}
	return summary, nil
}

// DeleteFiles deletes every uploaded file; their IDs stop working.
func (s *Service) DeleteFiles() (int, error) {
	files, err := s.c.listFiles()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, f := range files {
		if err := s.c.deleteFile(f.ID); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
