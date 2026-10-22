package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const connectorCacheLimit = 8
const connectorCacheTTL = 10 * time.Minute

// A connector describes immutable tools; execution still belongs to a task's
// sandbox and native call IDs. Holders are durable leases, never a router.
type cachedConnector struct {
	Instance, Key, Scope, Name, ID string
	Phase                          string
	Used                           time.Time
	DisconnectRequested            bool
	Holders                        map[string]bool
}

type connectorPool struct {
	mu      sync.Mutex // Never acquired while gateway.mu is held.
	g       *gateway
	scope   string
	entries map[string]*cachedConnector
	// Network operations are serialized only within one immutable catalog.
	// Waiters can cancel without waiting for another catalog's remote request.
	working map[string]chan struct{}
	wake    chan struct{}
}

func (p *connectorPool) path() string { return filepath.Join(p.g.dir, "gateway-connectors.json") }
func (p *connectorPool) schema(e *cachedConnector) string {
	return filepath.Join(p.g.dir, "gateway-connector-schema-"+e.Instance+".json")
}
func (p *connectorPool) save() error {
	return saveJSON(p.path(), object{"version": 1, "entries": p.entries})
}

func loadConnectorPool(g *gateway) (*connectorPool, error) {
	p := &connectorPool{g: g, scope: apiHash([]string{g.c.session.AccountID, g.c.session.DeviceID}), entries: map[string]*cachedConnector{}, working: map[string]chan struct{}{}, wake: make(chan struct{}, 1)}
	data, err := os.ReadFile(p.path())
	if err == nil {
		var saved struct {
			Version int
			Entries map[string]*cachedConnector
		}
		if json.Unmarshal(data, &saved) != nil || saved.Version != 1 || saved.Entries == nil || len(saved.Entries) > connectorCacheLimit {
			return nil, errors.New("invalid connector lease registry")
		}
		p.entries = saved.Entries
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for key, e := range p.entries {
		if e == nil || len(key) != 32 || strings.Trim(key, "0123456789abcdef") != "" || e.Instance != key || e.Name != connectorName(key) || len(e.Key) != 64 || len(e.Scope) != 64 {
			return nil, errors.New("invalid cached connector identity")
		}
		if e.Phase != "ready" && e.Phase != "creating" && e.Phase != "retiring" {
			return nil, errors.New("invalid cached connector state")
		}
		e.Holders = map[string]bool{}
		if e.Phase == "creating" {
			e.Phase = "retiring" // A creation with an unknown outcome is never repeated.
		}
	}
	// Derive ownership from task checkpoints, not potentially stale refcounts.
	paths, err := filepath.Glob(filepath.Join(g.dir, "gateway-state-*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var task object
		if json.Unmarshal(data, &task) != nil {
			return nil, errors.New("invalid task connector lease")
		}
		lease, id := textValue(task["connector_lease"]), textValue(task["id"])
		if lease == "" || task["cleanup_confirmed"] == true {
			continue
		}
		e := p.entries[lease]
		if e == nil {
			if task["done"] == true {
				continue // A released lease may predate its cleanup checkpoint.
			}
			return nil, errors.New("active task connector lease is missing")
		}
		creationGap := number(task["generation_posts"]) == 0 && textValue(task["connector_id"]) == ""
		if e.Scope != p.scope || filepath.Base(path) != "gateway-state-"+id+".json" || textValue(task["connector_label"]) != e.Name || !creationGap && textValue(task["connector_id"]) != e.ID {
			return nil, errors.New("task connector lease identity changed")
		}
		e.Holders[id] = true
	}
	if err = p.save(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *connectorPool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *connectorPool) attach(t *gatewayTurn, e *cachedConnector) error {
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	t.connectorLease, t.connectorLabel, t.connector = e.Instance, e.Name, e.ID
	return p.g.checkpointLocked(t)
}

func (p *connectorPool) list(ctx context.Context) ([]any, error) {
	check, cancel := context.WithTimeout(ctx, connectorCheckTimeout)
	defer cancel()
	return p.g.c.connectorList(check)
}

func (p *connectorPool) begin(ctx context.Context, key string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		wait := p.working[key]
		if wait == nil {
			p.working[key] = make(chan struct{})
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

func (p *connectorPool) end(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.working[key])
	delete(p.working, key)
}

func (p *connectorPool) acquire(t *gatewayTurn, tools []any) (string, bool, error) {
	program := p.g.helperPath
	if program == "" {
		var err error
		program, err = os.Executable()
		if err != nil {
			return "", true, err
		}
	}
	key := apiHash(object{"version": 1, "scope": p.scope, "program": program, "catalog": tools})
	if err := p.begin(t.ctx, key); err != nil {
		return "", true, err
	}
	defer func() {
		p.end(key)
		p.signal() // A release may have made this catalog eligible during its check.
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]*cachedConnector, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	for _, e := range entries {
		if e.Key != key || e.Scope != p.scope {
			continue
		}
		if e.Phase == "creating" {
			return "", true, errors.New("cached connector creation is unconfirmed; creation was not repeated")
		}
		if e.Phase != "ready" {
			continue
		}
		if len(e.Holders) == 0 && time.Since(e.Used) > connectorCacheTTL {
			e.Phase = "retiring"
			if err := p.save(); err != nil {
				return "", true, err
			}
			p.signal()
			continue
		}
		// This catalog stays reserved while the pool lock is released. A
		// reaper cannot select it and another catalog can make progress.
		p.mu.Unlock()
		rows, err := p.list(t.ctx)
		p.mu.Lock()
		if err != nil {
			return "", true, errors.New("cached connector availability could not be checked")
		}
		if err = t.ctx.Err(); err != nil {
			return "", true, err
		}
		if e.Phase != "ready" {
			// A failed holder can retire its connector during our check.
			return "", true, errors.New("cached connector was retired while checking availability")
		}
		available := false
		for _, row := range rows {
			if textValue(field(row, "connector_id")) == e.ID && textValue(field(row, "name")) == e.Name && (field(row, "enabled") == true || number(field(row, "enabled")) == 1) {
				available = true
			}
		}
		if !available {
			if len(e.Holders) > 0 {
				return "", true, errors.New("leased connector is unavailable; active tasks were not restarted")
			}
			e.Phase = "retiring"
			if err = p.save(); err != nil {
				return "", true, err
			}
			p.signal()
			continue
		}
		e.Holders[t.id], e.Used = true, time.Now()
		if err = p.save(); err != nil {
			delete(e.Holders, t.id)
			return "", true, err
		}
		if err = p.attach(t, e); err != nil {
			return e.ID, true, err
		}
		timing(t, "connector_cache_hit", time.Time{})
		return e.ID, true, nil
	}
	if len(p.entries) >= connectorCacheLimit {
		p.signal()
		timing(t, "connector_cache_full", time.Time{})
		return "", false, nil // The existing per-task path keeps capacity bounded.
	}
	instance := token()
	e := &cachedConnector{Instance: instance, Key: key, Scope: p.scope, Name: connectorName(instance), Phase: "creating", Used: time.Now(), Holders: map[string]bool{t.id: true}}
	descriptions := []any{}
	for _, tool := range tools {
		descriptions = append(descriptions, object{"name": field(tool, "name"), "description": field(tool, "description"), "inputSchema": parsed(field(tool, "inputSchema"))})
	}
	if err := saveJSON(p.schema(e), descriptions); err != nil {
		return "", true, err
	}
	p.entries[instance] = e
	if err := p.save(); err != nil {
		delete(p.entries, instance)
		_ = os.Remove(p.schema(e))
		return "", true, err
	}
	if err := p.attach(t, e); err != nil {
		delete(e.Holders, t.id)
		e.Phase = "retiring"
		_ = p.save()
		p.signal()
		return "", true, err
	}
	// The unique name and creation intent are durable before the one POST.
	name, schema := e.Name, p.schema(e)
	p.mu.Unlock()
	create, cancel := context.WithTimeout(t.ctx, 20*time.Second)
	result, createErr := p.g.c.office(create, "/alice/office/skills/manage/connector/create_personal", object{"name": name, "mcp_config": object{"transport_type": 3, "stdio_config": object{"command": program, "params": []string{"-action", "mcp-describe", "-tool-schema", schema}, "env": object{}}}})
	cancel()
	id := textValue(field(result, "data", "connector", "connector_id"))
	if id == "" {
		rows, _ := p.list(t.ctx)
		for _, row := range rows {
			if field(row, "name") == name {
				id = textValue(field(row, "connector_id"))
			}
		}
	}
	p.mu.Lock()
	e.ID = id
	if err := p.save(); err != nil {
		return e.ID, true, err
	}
	if err := p.attach(t, e); err != nil {
		return e.ID, true, err
	}
	if e.ID == "" {
		return "", true, errors.New("cached connector creation was not confirmed; creation was not retried")
	}
	if createErr != nil {
		return e.ID, true, createErr
	}
	p.mu.Unlock()
	err := p.g.c.awaitConnectorEnabled(t.ctx, id)
	p.mu.Lock()
	if err != nil {
		return e.ID, true, err
	}
	e.Phase = "ready"
	if err := p.save(); err != nil {
		return e.ID, true, err
	}
	timing(t, "connector_cache_miss", time.Time{})
	return e.ID, true, nil
}

func (p *connectorPool) release(t *gatewayTurn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.g.mu.Lock()
	done, failed := t.done, t.err != nil
	p.g.mu.Unlock()
	if !done {
		return errors.New("cannot release an unfinished task's connector")
	}
	if e := p.entries[t.connectorLease]; e != nil {
		if e.Scope != p.scope || e.Name != t.connectorLabel || t.connector != "" && e.ID != t.connector {
			return errors.New("connector lease release identity changed")
		}
		if e.Holders[t.id] {
			delete(e.Holders, t.id)
			e.Used = time.Now()
		}
		if failed || e.Phase != "ready" {
			e.Phase = "retiring"
		}
		if err := p.save(); err != nil {
			// Keep it pinned in memory too; a later recovery recomputes holders.
			e.Holders[t.id] = true
			return err
		}
	}
	p.g.mu.Lock()
	requested, confirmed := t.cleanupRequested, t.cleanupConfirmed
	t.cleanupRequested, t.cleanupConfirmed = true, true // The lease is released; the idle cache may remain.
	err := p.g.checkpointLocked(t)
	if err != nil {
		t.cleanupRequested, t.cleanupConfirmed = requested, confirmed
	}
	p.g.mu.Unlock()
	if err == nil {
		timing(t, "connector_lease_released", time.Time{})
	}
	p.signal()
	return err
}

func (p *connectorPool) start() {
	p.g.wg.Add(1)
	go func() {
		defer p.g.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.g.ctx.Done():
				return
			case <-ticker.C:
			case <-p.wake:
			}
			ctx, cancel := context.WithTimeout(p.g.ctx, 5*time.Second)
			if err := p.reap(ctx, false); err != nil && p.g.ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "GATEWAY_CONNECTOR_CACHE_CLEANUP_PENDING")
			}
			cancel()
		}
	}()
	p.signal()
}

func (p *connectorPool) close() {
	// Join task workers before draining: a finished worker may still be
	// releasing its lease when shutdown starts. Suspended live leases survive.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.reap(ctx, true); err != nil {
		fmt.Fprintln(os.Stderr, "GATEWAY_CONNECTOR_CACHE_DRAIN_PENDING")
	}
}

func (p *connectorPool) reap(ctx context.Context, drain bool) error {
	p.mu.Lock()
	var oldest *cachedConnector
	if len(p.entries) >= connectorCacheLimit {
		for _, e := range p.entries {
			if e.Scope == p.scope && len(e.Holders) == 0 && p.working[e.Key] == nil && (oldest == nil || e.Used.Before(oldest.Used)) {
				oldest = e
			}
		}
	}
	candidates := []*cachedConnector{}
	for _, e := range p.entries {
		if e.Scope != p.scope || len(e.Holders) != 0 || !drain && e.Phase == "ready" && time.Since(e.Used) <= connectorCacheTTL && e != oldest {
			continue
		}
		candidates = append(candidates, e)
	}
	p.mu.Unlock()
	for _, e := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.mu.Lock()
		// Recheck after dropping the lock: an acquire may now own a lease
		// or be checking this catalog. Cleanup never waits behind it.
		if p.entries[e.Instance] != e || len(e.Holders) != 0 || p.working[e.Key] != nil || !drain && e.Phase == "ready" && time.Since(e.Used) <= connectorCacheTTL && e != oldest {
			p.mu.Unlock()
			continue
		}
		p.working[e.Key] = make(chan struct{})
		p.mu.Unlock()
		err := p.retire(ctx, e)
		p.end(e.Key)
		if err != nil {
			return err
		}
	}
	return nil
}

// The catalog operation is reserved, with no holders. The pool lock covers
// only state and checkpoints, never remote listing, disconnect or readback.
// An uncertain disconnect is read back, never sent again.
func (p *connectorPool) retire(ctx context.Context, e *cachedConnector) error {
	p.mu.Lock()
	e.Phase = "retiring"
	if err := p.save(); err != nil {
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	for {
		rows, err := p.list(ctx)
		if err != nil {
			return err
		}
		p.mu.Lock()
		present := false
		for _, row := range rows {
			if e.ID != "" && textValue(field(row, "connector_id")) == e.ID || e.ID == "" && textValue(field(row, "name")) == e.Name {
				if textValue(field(row, "name")) != e.Name {
					p.mu.Unlock()
					return errors.New("cached connector name changed before cleanup")
				}
				e.ID, present = textValue(field(row, "connector_id")), true
			}
		}
		if !present {
			if e.ID == "" && time.Since(e.Used) < 30*time.Second {
				p.mu.Unlock()
				return nil // Allow delayed listing of an uncertain creation.
			}
			delete(p.entries, e.Instance)
			if err = p.save(); err != nil {
				p.entries[e.Instance] = e
				p.mu.Unlock()
				return err
			}
			p.mu.Unlock()
			_ = os.Remove(p.schema(e))
			return nil
		}
		disconnect := !e.DisconnectRequested
		if !e.DisconnectRequested {
			e.DisconnectRequested = true
			if err = p.save(); err != nil {
				e.DisconnectRequested = false
				p.mu.Unlock()
				return err
			}
		}
		id := e.ID
		p.mu.Unlock()
		if disconnect {
			_, _ = p.g.c.office(ctx, "/alice/office/skills/manage/connector/disconnect", object{"connector_id": id, "skill_type": 1})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
