package doubao

import (
	"context"
	"encoding/json"
	"errors"
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

type connectorCacheTransport struct {
	mu                     sync.Mutex
	rows                   map[string]object
	creates                int
	disconnects            map[string]int
	listError, createError bool
	disconnectError        bool
	numericID              bool
}

func (f *connectorCacheTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows, f.disconnects = map[string]object{}, map[string]int{}
	}
	data := object{}
	switch r.URL.Path {
	case "/alice/office/skills/manage/connector/create_personal":
		var body object
		if err := decode(r.Body, &body); err != nil {
			return nil, err
		}
		f.creates++
		id := fmt.Sprintf("cached-%d", f.creates)
		var nativeID any = id
		if f.numericID {
			id = fmt.Sprint(9000 + f.creates)
			nativeID = json.Number(id)
		}
		f.rows[id] = object{"connector_id": nativeID, "name": body["name"], "enabled": true}
		if f.createError {
			return nil, errors.New("fixture uncertain creation")
		}
		data["connector"] = object{"connector_id": nativeID}
	case "/alice/office/skills/manage/connector/list_by_user":
		if f.listError {
			return nil, errors.New("fixture listing unavailable")
		}
		items := []any{}
		for _, row := range f.rows {
			items = append(items, row)
		}
		data["items"], data["has_more"] = items, false
	case "/alice/office/skills/manage/connector/disconnect":
		var body object
		if err := decode(r.Body, &body); err != nil {
			return nil, err
		}
		id := textValue(body["connector_id"])
		f.disconnects[id]++
		if f.disconnectError {
			return nil, errors.New("fixture uncertain disconnect")
		}
		delete(f.rows, id)
	default:
		return nil, fmt.Errorf("unexpected cache request: %s", r.URL.Path)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jsonText(object{"code": 0, "data": data})))}, nil
}

func cacheFixture(t *testing.T) (*gateway, *connectorCacheTransport) {
	t.Helper()
	g, _ := testGateway(t)
	f := &connectorCacheTransport{}
	g.c.http.Transport = f
	var err error
	g.connectors, err = loadConnectorPool(g)
	if err != nil {
		t.Fatal(err)
	}
	return g, f
}

func cacheTurn(t *testing.T, g *gateway) *gatewayTurn {
	t.Helper()
	id := token()
	ctx, cancel := context.WithCancel(g.ctx)
	t.Cleanup(cancel)
	x := newExchange("TestModel")
	turn := &gatewayTurn{id: id, ctx: ctx, cancel: cancel, started: time.Now(), request: toolRequest(), first: x, current: x, statePath: filepath.Join(g.dir, "gateway-state-"+id+".json"), receiptPath: filepath.Join(g.dir, "gateway-receipt-"+id+".json")}
	g.turns[id] = turn
	return turn
}

func releaseCacheTurn(t *testing.T, g *gateway, turn *gatewayTurn) {
	t.Helper()
	g.mu.Lock()
	turn.done = true
	g.mu.Unlock()
	if err := g.removeConnector(turn); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorCacheSharesOnlyCatalogAndKeepsActiveLeases(t *testing.T) {
	g, f := cacheFixture(t)
	a, b := cacheTurn(t, g), cacheTurn(t, g)
	tools := toolCatalog(a.request)
	id, err := g.newConnector(a, tools)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.newConnector(b, tools)
	if err != nil || second != id || a.connectorLease != b.connectorLease || a.connectorLabel != b.connectorLabel {
		t.Fatal("identical immutable catalog was not reused", err)
	}
	if a.id == b.id || a.statePath == b.statePath || f.creates != 1 {
		t.Fatal("tasks were merged or creation repeated")
	}
	releaseCacheTurn(t, g, a)
	if err = g.connectors.reap(context.Background(), true); err != nil || len(f.rows) != 1 || f.disconnects[id] != 0 {
		t.Fatal("one task removed another task's live connector", err)
	}
	c := cacheTurn(t, g)
	c.request.Tools[0].Function.Description = "a different complete definition"
	third, err := g.newConnector(c, toolCatalog(c.request))
	if err != nil || third == id || f.creates != 2 {
		t.Fatal("changed catalog reused old connector", err)
	}
	releaseCacheTurn(t, g, b)
	releaseCacheTurn(t, g, c)
	if err = g.connectors.reap(context.Background(), true); err != nil || len(f.rows) != 0 || len(g.connectors.entries) != 0 {
		t.Fatal("idle connectors were not drained", err)
	}
	for _, count := range f.disconnects {
		if count != 1 {
			t.Fatal("disconnect repeated", count)
		}
	}
}

func TestConnectorCacheRechecksMissingDisabledAndUnknownAvailability(t *testing.T) {
	g, f := cacheFixture(t)
	a := cacheTurn(t, g)
	id, err := g.newConnector(a, toolCatalog(a.request))
	if err != nil {
		t.Fatal(err)
	}
	f.rows[id]["enabled"] = false
	b := cacheTurn(t, g)
	if _, err = g.newConnector(b, toolCatalog(b.request)); err == nil || f.creates != 1 || b.connectorLease != "" {
		t.Fatal("unavailable active connector was replaced or leased")
	}
	releaseCacheTurn(t, g, a)
	delete(f.rows, id)
	f.listError = true
	if _, err = g.newConnector(b, toolCatalog(b.request)); err == nil || f.creates != 1 {
		t.Fatal("uncertain listing caused another create")
	}
	f.listError = false
	newID, err := g.newConnector(b, toolCatalog(b.request))
	if err != nil || newID == id || f.creates != 2 {
		t.Fatal("missing idle connector was not rebuilt", err)
	}
	if err = g.connectors.reap(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(g.dir, "gateway-connector-schema-"+a.connectorLease+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retired descriptor was kept", err)
	}
	releaseCacheTurn(t, g, b)
}

func TestConnectorCacheRecoveryDerivesLeasesAndToleratesCreationCheckpointGap(t *testing.T) {
	g, f := cacheFixture(t)
	a, b := cacheTurn(t, g), cacheTurn(t, g)
	id, err := g.newConnector(a, toolCatalog(a.request))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.newConnector(b, toolCatalog(b.request)); err != nil {
		t.Fatal(err)
	}
	releaseCacheTurn(t, g, a)
	entry := g.connectors.entries[b.connectorLease]
	// Simulate the journal having an ID before the generation-free task does.
	b.connector = ""
	if err = g.checkpointLocked(b); err != nil {
		t.Fatal(err)
	}
	entry.Holders[a.id] = true // Stale refcount must not outlive the cleanup checkpoint.
	if err = g.connectors.save(); err != nil {
		t.Fatal(err)
	}
	restored, err := loadConnectorPool(g)
	if err != nil {
		t.Fatal(err)
	}
	entry = restored.entries[b.connectorLease]
	if len(entry.Holders) != 1 || !entry.Holders[b.id] {
		t.Fatal("recovered holder count was trusted rather than derived")
	}
	g.connectors = restored
	if err = restored.reap(context.Background(), true); err != nil || f.disconnects[id] != 0 {
		t.Fatal("recovery disconnected a suspended lease", err)
	}
	b.connector = id
	releaseCacheTurn(t, g, b)
	if err = restored.reap(context.Background(), true); err != nil || f.creates != 1 || f.disconnects[id] != 1 {
		t.Fatal("recovery repeated a create or failed cleanup", err)
	}
}

func TestConnectorCacheUncertainMutationsAreNeverResent(t *testing.T) {
	g, f := cacheFixture(t)
	f.createError = true
	a := cacheTurn(t, g)
	id, err := g.newConnector(a, toolCatalog(a.request))
	if err == nil || id == "" || f.creates != 1 || a.connector != id {
		t.Fatal("uncertain create was repeated or not reconciled", err)
	}
	a.err = apiFail(429, "task_not_started", "fixture")
	releaseCacheTurn(t, g, a)
	f.disconnectError = true
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err = g.connectors.reap(ctx, true)
		cancel()
		if err == nil || f.disconnects[id] != 1 || f.creates != 1 {
			t.Fatal("uncertain mutation was retried", err, f.disconnects[id])
		}
	}
	delete(f.rows, id) // Later readback confirms the one disconnect completed.
	if err = g.connectors.reap(context.Background(), true); err != nil || len(g.connectors.entries) != 0 {
		t.Fatal("confirmed delayed removal was not recorded", err)
	}
}

func TestConnectorCacheExpiryCannotRemoveAHolderAndScopeIsPartOfTheKey(t *testing.T) {
	g, f := cacheFixture(t)
	a := cacheTurn(t, g)
	id, err := g.newConnector(a, toolCatalog(a.request))
	if err != nil {
		t.Fatal(err)
	}
	entry := g.connectors.entries[a.connectorLease]
	entry.Used = time.Now().Add(-2 * connectorCacheTTL)
	if err = g.connectors.reap(context.Background(), false); err != nil || f.disconnects[id] != 0 {
		t.Fatal("TTL removed a live lease", err)
	}
	releaseCacheTurn(t, g, a)
	entry.Used = time.Now().Add(-2 * connectorCacheTTL)
	b := cacheTurn(t, g)
	newID, err := g.newConnector(b, toolCatalog(b.request))
	if err != nil || newID == id {
		t.Fatal("expired entry was reused", err)
	}
	g.connectors.scope = apiHash("a different account and device")
	c := cacheTurn(t, g)
	third, err := g.newConnector(c, toolCatalog(c.request))
	if err != nil || third == newID || f.creates != 3 {
		t.Fatal("account/device scope shared a connector", err)
	}
}

func TestConnectorCacheBoundsIdleCatalogsAndFallsBackWithoutDroppingLiveLeases(t *testing.T) {
	g, f := cacheFixture(t)
	var first *gatewayTurn
	for i := range connectorCacheLimit {
		turn := cacheTurn(t, g)
		turn.request.Tools[0].Function.Description = fmt.Sprintf("unique complete catalog %d", i)
		if _, err := g.newConnector(turn, toolCatalog(turn.request)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = turn
		} else {
			releaseCacheTurn(t, g, turn)
		}
	}
	extra := cacheTurn(t, g)
	extra.request.Tools[0].Function.Description = "catalog beyond the cache bound"
	if _, err := g.newConnector(extra, toolCatalog(extra.request)); err != nil || extra.connectorLease != "" || len(g.connectors.entries) != connectorCacheLimit {
		t.Fatal("bounded cache did not use dedicated fallback", err)
	}
	if err := g.connectors.reap(context.Background(), false); err != nil || f.disconnects[first.connector] != 0 || len(g.connectors.entries) != connectorCacheLimit-1 {
		t.Fatal("capacity eviction removed a holder or did not evict idle LRU", err)
	}
	releaseCacheTurn(t, g, first)
	releaseCacheTurn(t, g, extra)
	if err := g.connectors.reap(context.Background(), true); err != nil || len(f.rows) != 0 {
		t.Fatal("bounded cache drain left connectors", err)
	}
}

func TestConnectorCacheRejectsMissingOrChangedActiveLeaseRegistry(t *testing.T) {
	g, f := cacheFixture(t)
	a := cacheTurn(t, g)
	if _, err := g.newConnector(a, toolCatalog(a.request)); err != nil {
		t.Fatal(err)
	}
	e := g.connectors.entries[a.connectorLease]
	delete(g.connectors.entries, a.connectorLease)
	if err := g.connectors.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConnectorPool(g); err == nil {
		t.Fatal("missing live registry silently started a new connector")
	}
	g.connectors.entries[a.connectorLease] = e
	if err := g.connectors.save(); err != nil {
		t.Fatal(err)
	}
	a.connectorLabel = "changed connector identity"
	if err := g.checkpointLocked(a); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConnectorPool(g); err == nil || f.creates != 1 || len(f.disconnects) != 0 {
		t.Fatal("changed live registry caused mutation instead of explicit failure")
	}
}

func TestConnectorCacheServiceCloseJoinsReleasesAndPreservesSuspendedTasks(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		t.Run(fmt.Sprint("suspended=", suspended), func(t *testing.T) {
			g, f := cacheFixture(t)
			ctx, cancel := context.WithCancel(g.ctx)
			g.ctx = ctx
			turn := cacheTurn(t, g)
			if _, err := g.newConnector(turn, toolCatalog(turn.request)); err != nil {
				t.Fatal(err)
			}
			g.connectors.start()
			if !suspended {
				g.wg.Add(1)
				go func() {
					defer g.wg.Done()
					<-ctx.Done()
					g.mu.Lock()
					turn.done = true
					g.mu.Unlock()
					if err := g.removeConnector(turn); err != nil {
						t.Error(err)
					}
				}()
			}
			service := &Service{g: g, c: g.c, cancel: cancel}
			service.Close()
			remaining := 0
			if suspended {
				remaining = 1
			}
			if len(f.rows) != remaining || len(g.connectors.entries) != remaining || turn.cleanupConfirmed == suspended {
				t.Fatal("shutdown lost a suspended lease or leaked a joined release")
			}
			if _, err := loadConnectorPool(g); err != nil {
				t.Fatal("shutdown left an unrecoverable registry", err)
			}
		})
	}
}

func TestConnectorCacheNumericIDsKeepAvailabilityAndCleanup(t *testing.T) {
	g, f := cacheFixture(t)
	f.numericID = true
	a, b := cacheTurn(t, g), cacheTurn(t, g)
	id, err := g.newConnector(a, toolCatalog(a.request))
	if err != nil {
		t.Fatal(err)
	}
	other, err := g.newConnector(b, toolCatalog(b.request))
	if err != nil || other != id || f.creates != 1 {
		t.Fatal("numeric connector ID was not reused", err)
	}
	releaseCacheTurn(t, g, a)
	releaseCacheTurn(t, g, b)
	if err = g.connectors.reap(context.Background(), true); err != nil || f.disconnects[id] != 1 || len(f.rows) != 0 {
		t.Fatal("numeric ID cleanup was falsely confirmed", err)
	}
}
