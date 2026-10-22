package doubao

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold each kind of remote operation indefinitely: another catalog must finish
// before it is released. This tests isolation without a machine-speed claim.
func TestConnectorCacheNetworkWaitsDoNotBlockOtherCatalogs(t *testing.T) {
	for _, operation := range []string{"create", "availability", "disconnect"} {
		t.Run(operation, func(t *testing.T) {
			g, f := cacheFixture(t)
			seed, otherSeed := cacheTurn(t, g), cacheTurn(t, g)
			primary, peer, canceled, warm := cacheTurn(t, g), cacheTurn(t, g), cacheTurn(t, g), cacheTurn(t, g)
			for _, turn := range []*gatewayTurn{otherSeed, primary, peer, canceled} {
				turn.request.Tools[0].Function.Description = "A separate complete immutable catalog."
			}
			hotID, err := g.newConnector(seed, toolCatalog(seed.request))
			if err != nil {
				t.Fatal(err)
			}
			releaseCacheTurn(t, g, seed)
			if operation != "create" {
				if _, err = g.newConnector(otherSeed, toolCatalog(otherSeed.request)); err != nil {
					t.Fatal(err)
				}
				releaseCacheTurn(t, g, otherSeed)
				if operation == "disconnect" {
					g.connectors.entries[otherSeed.connectorLease].Used = time.Now().Add(-2 * connectorCacheTTL)
				}
			}
			paths := map[string]string{"create": "create_personal", "availability": "list_by_user", "disconnect": "disconnect"}
			entered, unblock := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(unblock) }) }
			defer release()
			var blocked atomic.Bool
			g.c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/alice/office/skills/manage/connector/"+paths[operation] && blocked.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-unblock:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return f.RoundTrip(r)
			})
			primaryDone := make(chan error, 1)
			go func() {
				if operation == "disconnect" {
					primaryDone <- g.connectors.reap(context.Background(), false)
				} else {
					_, err := g.newConnector(primary, toolCatalog(primary.request))
					primaryDone <- err
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("fixture did not enter the held remote operation")
			}
			warmDone := make(chan error, 1)
			go func() {
				id, err := g.newConnector(warm, toolCatalog(warm.request))
				if err == nil && id != hotID {
					err = errors.New("warm catalog was replaced")
				}
				warmDone <- err
			}()
			select {
			case err = <-warmDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("unrelated warm catalog waited behind a remote operation")
			}
			select {
			case <-primaryDone:
				t.Fatal("held remote operation completed before being released")
			default:
			}
			var peerDone chan error
			if operation == "create" {
				peerDone = make(chan error, 1)
				go func() { _, err := g.newConnector(peer, toolCatalog(peer.request)); peerDone <- err }()
				cancelDone, started := make(chan error, 1), make(chan struct{})
				go func() {
					close(started)
					_, err := g.newConnector(canceled, toolCatalog(canceled.request))
					cancelDone <- err
				}()
				<-started
				canceled.cancel()
				select {
				case err = <-cancelDone:
					if !errors.Is(err, context.Canceled) || canceled.connectorLease != "" {
						t.Fatal("canceled waiter acquired a lease or lost cancellation", err)
					}
				case <-time.After(time.Second):
					t.Fatal("same-catalog waiter could not cancel")
				}
			}
			// Concurrent reapers skip reserved catalogs and never disconnect them.
			if err = g.connectors.reap(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			release()
			for _, done := range []chan error{primaryDone, peerDone} {
				if done == nil {
					continue
				}
				select {
				case err = <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("released catalog did not finish")
				}
			}
			if f.creates != 2 || operation == "create" && (peer.connector != primary.connector || peer.connectorLease != primary.connectorLease) {
				t.Fatal("concurrent same-catalog requests repeated registration")
			}
			if operation != "disconnect" {
				releaseCacheTurn(t, g, primary)
			}
			if peerDone != nil {
				releaseCacheTurn(t, g, peer)
			}
			releaseCacheTurn(t, g, warm)
			if err = g.connectors.reap(context.Background(), true); err != nil || len(f.rows) != 0 {
				t.Fatal("concurrent operations left idle connectors", err)
			}
		})
	}
}

func TestConnectorCacheFailedCreationIsNotRepeatedBySameCatalogWaiter(t *testing.T) {
	g, f := cacheFixture(t)
	f.createError = true
	a, b := cacheTurn(t, g), cacheTurn(t, g)
	if _, err := g.newConnector(a, toolCatalog(a.request)); err == nil {
		t.Fatal("uncertain creation was accepted")
	}
	if _, err := g.newConnector(b, toolCatalog(b.request)); err == nil || f.creates != 1 || b.connectorLease != "" {
		t.Fatal("unconfirmed creation was repeated by a waiter")
	}
	a.err = apiFail(503, "fixture", "uncertain creation")
	releaseCacheTurn(t, g, a)
	if err := g.connectors.reap(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}
