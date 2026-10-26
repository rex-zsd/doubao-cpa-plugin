package doubao

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// The plugin page shows how tasks run and end. It reads metadata only: no
// prompt, tool argument, tool result or answer leaves this file.

// taskOutcome is how one task ended.
type taskOutcome struct {
	at       time.Time
	code     string
	mode     string
	reissues int
}

// outcomeLimit bounds the outcome history the page summarizes.
const outcomeLimit = 512

func (g *gateway) recordOutcomeLocked(t *gatewayTurn, failure *apiError) {
	code := "ok"
	if failure != nil {
		code = failure.Code
	}
	g.outcomes = append(g.outcomes, taskOutcome{at: time.Now(), code: code, mode: t.mode, reissues: t.reissues})
	if len(g.outcomes) > outcomeLimit {
		g.outcomes = append([]taskOutcome(nil), g.outcomes[len(g.outcomes)-outcomeLimit:]...)
	}
}

// TaskInfo describes a task for the plugin page.
type TaskInfo struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	Mode      string    `json:"mode"`
	State     string    `json:"state"`
	Tool      string    `json:"tool,omitempty"`
	WaitingMS int64     `json:"waiting_ms,omitempty"`
	Started   time.Time `json:"started"`
	Deadline  time.Time `json:"deadline,omitzero"`
	Forwarded int       `json:"forwarded"`
	Reissues  int       `json:"reissues"`
	Outcome   string    `json:"outcome,omitempty"`
	Finished  time.Time `json:"finished,omitzero"`
}

// Tasks lists running tasks first, then those finished in the last half hour,
// newest first within each group.
func (s *Service) Tasks() []TaskInfo {
	g := s.g
	g.mu.Lock()
	defer g.mu.Unlock()
	running, finished := []TaskInfo{}, []TaskInfo{}
	for _, t := range g.turns {
		info := TaskInfo{ID: t.id, Model: t.request.Model, Mode: t.mode, State: "running", Started: t.started, Forwarded: t.forwarded, Reissues: t.reissues}
		if info.Mode == "" {
			info.Mode = "work"
		}
		if t.done {
			info.State, info.Outcome, info.Finished = "done", "ok", t.finished
			if t.err != nil {
				info.Outcome = t.err.Code
			}
			finished = append(finished, info)
			continue
		}
		info.Deadline = t.deadline
		for _, p := range g.calls {
			if p.turn != t || p.submitted || p.exposedAt.IsZero() {
				continue
			}
			names := []string{p.call.Function.Name}
			if len(p.batchCalls) > 0 {
				names = names[:0]
				for _, call := range p.batchCalls {
					names = append(names, call.Function.Name)
				}
			}
			info.State, info.Tool, info.WaitingMS = "waiting_tool", strings.Join(names, ", "), time.Since(p.exposedAt).Milliseconds()
			break
		}
		running = append(running, info)
	}
	newest := func(list []TaskInfo, at func(TaskInfo) time.Time) {
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && at(list[j]).After(at(list[j-1])); j-- {
				list[j], list[j-1] = list[j-1], list[j]
			}
		}
	}
	newest(running, func(t TaskInfo) time.Time { return t.Started })
	newest(finished, func(t TaskInfo) time.Time { return t.Finished })
	return append(running, finished...)
}

// Stats summarizes the gateway for the plugin page.
type Stats struct {
	Active      int            `json:"active"`
	Limit       int            `json:"limit"`
	LastRequest time.Time      `json:"last_request_at,omitzero"`
	WindowHours int            `json:"window_hours"`
	Finished    int            `json:"finished"`
	Outcomes    map[string]int `json:"outcomes"`
	Reissues    int            `json:"deduplicated_reissues"`
	Resumes     int64          `json:"resumed_streams"`
}

func (s *Service) Stats() Stats {
	g := s.g
	g.mu.Lock()
	defer g.mu.Unlock()
	stats := Stats{Limit: activeTaskLimit, LastRequest: g.lastRequest, WindowHours: 24, Outcomes: map[string]int{}, Resumes: s.c.resumes.Load()}
	for _, t := range g.turns {
		if !t.done && t.abort == nil {
			stats.Active++
		}
	}
	since := time.Now().Add(-24 * time.Hour)
	for _, o := range g.outcomes {
		if o.at.Before(since) {
			continue
		}
		stats.Finished++
		stats.Outcomes[o.code]++
		stats.Reissues += o.reissues
	}
	return stats
}

// CancelTask stops a running task as a newer request would: Work's run is
// stopped and the caller waiting on it gets task_cancelled. A tool result
// sent for it later starts a new task over the full conversation.
func (s *Service) CancelTask(id string) bool {
	g := s.g
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.turns[id]
	if t == nil || t.done || t.abort != nil {
		return false
	}
	g.supersedeLocked(t, apiFail(409, "task_cancelled", "The task was cancelled from the plugin page; resend the conversation to start a new task"))
	return true
}

func publicCatalog(aliases []Model) []PublicModel {
	public := []PublicModel{}
	for _, m := range aliases {
		public = append(public, PublicModel{ID: m.Name, DisplayName: m.Display, Window: m.Window, Efforts: m.Efforts})
	}
	return public
}

// catalog is the Work model list, which the plugin page can refresh.
func (g *gateway) catalog() []Model {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.models
}

// PublicModels lists the routable models.
func (s *Service) PublicModels() []PublicModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PublicModel(nil), s.models...)
}

// ModelsRefreshed is when the model list was last loaded from Work.
func (s *Service) ModelsRefreshed() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshed
}

// RefreshModels reloads the Work model list. Running tasks keep the model
// they selected; new requests see the new list.
func (s *Service) RefreshModels(ctx context.Context) (added, removed []string, err error) {
	models, err := s.c.models(ctx)
	if err != nil {
		return nil, nil, err
	}
	aliases := publicModels(models)
	if len(aliases) == 0 {
		return nil, nil, errors.New("no available Work models")
	}
	public := publicCatalog(aliases)
	s.mu.Lock()
	before := map[string]bool{}
	for _, m := range s.models {
		before[m.ID] = true
	}
	s.models, s.refreshed = public, time.Now()
	s.mu.Unlock()
	s.g.mu.Lock()
	s.g.models = aliases
	s.g.mu.Unlock()
	for _, m := range public {
		if !before[m.ID] {
			added = append(added, m.ID)
		}
		delete(before, m.ID)
	}
	for id := range before {
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return added, removed, nil
}

// SetNativeTools changes native routing for tasks started from now on; a
// running task keeps the policy it was saved with.
func (s *Service) SetNativeTools(enabled bool) {
	s.g.mu.Lock()
	s.g.nativeTools = enabled
	s.g.mu.Unlock()
}
