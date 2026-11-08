package doubao

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restartThreadGateway(t *testing.T, old *gateway) (*gateway, *httptest.Server, chan threadStart) {
	t.Helper()
	old.wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	g := newGateway(ctx, old.c, old.models, old.dir, time.Minute)
	g.nativeTools = old.nativeTools
	g.apiKey = ""
	if err := g.recoverTasks(); err != nil {
		cancel()
		t.Fatal(err)
	}
	starts := make(chan threadStart, 16)
	g.run = threadRunner(g, starts)
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); server.Close(); g.wg.Wait() })
	return g, server, starts
}

func completedThread(t *testing.T) (*gateway, apiRequest, string) {
	t.Helper()
	g, server := testGateway(t)
	starts := make(chan threadStart, 2)
	g.run = threadRunner(g, starts)
	req := chatRequest("Keep this original native context")
	status, response := postGateway(t, server, req, "initial-thread")
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	<-starts
	g.wg.Wait()
	answer := textValue(returnedMessage(t, response).Content)
	next := chatRequest(textValue(req.Messages[0].Content), answer, "Continue the original context")
	return g, next, answer
}

func TestDurableThreadSurvivesRestartAfterSourceTaskWasSwept(t *testing.T) {
	g, next, _ := completedThread(t)
	g.mu.Lock()
	for _, th := range g.threads {
		th.context = 43210
	}
	if err := g.saveThreadsLocked(); err != nil {
		t.Fatal(err)
	}
	for _, turn := range g.turns {
		g.dropTurnLocked(turn)
	}
	g.mu.Unlock()
	if paths, _ := filepath.Glob(filepath.Join(g.dir, "gateway-state-*.json")); len(paths) != 0 {
		t.Fatal("source task was not swept")
	}
	restored, server, starts := restartThreadGateway(t, g)
	status, response := postGateway(t, server, next, "after-restart")
	if status != 200 || (<-starts) != (threadStart{"conv-1", 2}) {
		t.Fatal("restart did not reuse original conversation", status, response)
	}
	restored.wg.Wait()
	if len(restored.turns) != 1 {
		t.Fatal("task receipts were required to restore the head")
	}
	for _, turn := range restored.turns {
		if turn.context != 43210 {
			t.Fatal("reported context floor was lost")
		}
	}
	data, err := os.ReadFile(restored.threadsPath())
	if err != nil || strings.Contains(string(data), "Keep this original native context") {
		t.Fatal("continuation index contains prompt text or was not saved", err)
	}
	info, _ := os.Stat(restored.threadsPath())
	if info.Mode().Perm() != 0600 {
		t.Fatal("continuation index is not private", info.Mode())
	}
}

func TestDurableThreadClaimIsConsumedBeforeRunnerAndSurvivesCrash(t *testing.T) {
	g, next, _ := completedThread(t)
	started := make(chan *gatewayTurn, 1)
	g.run = func(turn *gatewayTurn) { started <- turn }
	if _, failure := g.submit(next, "crash-before-dispatch"); failure != nil {
		t.Fatal(failure)
	}
	turn := <-started
	g.wg.Wait()
	defer turn.cancel()
	defer turn.timer.Stop()
	var saved savedGatewayThreads
	data, _ := os.ReadFile(g.threadsPath())
	if json.Unmarshal(data, &saved) != nil || len(saved.Heads) != 0 {
		t.Fatal("runner began before claim was durably consumed")
	}
	g.c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("recovery submitted unexpected network traffic")
		return nil, context.Canceled
	})
	restored, server, starts := restartThreadGateway(t, g)
	if len(restored.threads) != 0 {
		t.Fatal("crash resurrected consumed head")
	}
	status, response := postGateway(t, server, next, "new-attempt")
	if status != 200 || (<-starts).conversation != "" {
		t.Fatal("old branch was reused after interrupted claim", status, response)
	}
}

func TestDurableThreadLatestHeadOnlyAndConcurrentClaimOnce(t *testing.T) {
	g, next, _ := completedThread(t)
	restored, server, starts := restartThreadGateway(t, g)
	status, response := postGateway(t, server, next, "second-turn")
	if status != 200 || (<-starts).conversation != "conv-1" {
		t.Fatal(status, response)
	}
	second := returnedMessage(t, response)
	latest := next
	latest.Messages = append(append([]apiMessage{}, next.Messages...), second, apiMessage{Role: "user", Content: "third"})
	thirdGateway, _, thirdStarts := restartThreadGateway(t, restored)
	// Both callers extend the same head. Only one can advance it.
	results := make(chan *apiError, 2)
	for i := 0; i < 2; i++ {
		go func() { _, failure := thirdGateway.submit(latest, ""); results <- failure }()
	}
	for i := 0; i < 2; i++ {
		if failure := <-results; failure != nil {
			t.Fatal(failure)
		}
	}
	continued := 0
	for i := 0; i < 2; i++ {
		if got := <-thirdStarts; got.conversation == "conv-1" && got.continued == 4 {
			continued++
		}
	}
	if continued != 1 {
		t.Fatal("same native head was claimed multiple times", continued)
	}
	thirdGateway.wg.Wait()
	// A regeneration from the older history must not recover conv-1.
	if _, failure := thirdGateway.submit(next, "old-history"); failure != nil {
		t.Fatal(failure)
	}
	if got := <-thirdStarts; got.conversation != "" {
		t.Fatal("older consumed branch was still eligible", got)
	}
}

func TestDurableThreadClaimWriteFailureNeverStartsGeneration(t *testing.T) {
	g, next, _ := completedThread(t)
	backup := g.threadsPath() + ".saved"
	if err := os.Rename(g.threadsPath(), backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(g.threadsPath(), 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	g.run = func(*gatewayTurn) { calls.Add(1) }
	_, failure := g.submit(next, "blocked-index")
	if failure == nil || failure.Code != "state_persistence_failed" || calls.Load() != 0 || len(g.threads) != 1 {
		t.Fatal("failed durable claim advanced a conversation", failure, calls.Load())
	}
	if err := os.Remove(g.threadsPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, g.threadsPath()); err != nil {
		t.Fatal(err)
	}
	_, server, starts := restartThreadGateway(t, g)
	status, response := postGateway(t, server, next, "unblocked-index")
	if status != 200 || (<-starts).conversation != "conv-1" {
		t.Fatal("unsubmitted head was lost", status, response)
	}
}

func TestDurableThreadPublicationFailureKeepsCompletedAnswer(t *testing.T) {
	g, server := testGateway(t)
	if err := os.Mkdir(g.threadsPath(), 0700); err != nil {
		t.Fatal(err)
	}
	starts := make(chan threadStart, 1)
	g.run = threadRunner(g, starts)
	status, response := postGateway(t, server, chatRequest("publication-failure"), "saved-final")
	if status != 200 || returnedMessage(t, response).Content != "conv-1: publication-failure" {
		t.Fatal("optional reuse failure discarded completed answer", status, response)
	}
	<-starts
	g.wg.Wait()
	if len(g.threads) != 0 {
		t.Fatal("failed publication remained reusable")
	}
	if err := os.Remove(g.threadsPath()); err != nil {
		t.Fatal(err)
	}
	restored, _, _ := restartThreadGateway(t, g)
	if len(restored.threads) != 0 {
		t.Fatal("missing index was reconstructed from old receipts")
	}
	x, failure := restored.submit(chatRequest("publication-failure"), "saved-final")
	if failure != nil || x.message.Content != "conv-1: publication-failure" {
		t.Fatal("completed idempotent replay was lost", failure)
	}
}

func TestDurableThreadRecoveryRejectsStaleHeadAndMalformedIndex(t *testing.T) {
	for _, kind := range []string{"stale", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			g, next, _ := completedThread(t)
			previous, err := os.ReadFile(g.threadsPath())
			if err != nil {
				t.Fatal(err)
			}
			starts := make(chan threadStart, 1)
			g.run = threadRunner(g, starts)
			x, failure := g.submit(next, "advance-native")
			if failure != nil {
				t.Fatal(failure)
			}
			<-starts
			<-x.ready
			g.wg.Wait()
			if kind == "malformed" {
				previous = []byte(`{"version":1,"heads":null}`)
			}
			if err := os.WriteFile(g.threadsPath(), previous, 0600); err != nil {
				t.Fatal(err)
			}
			restored := newGateway(context.Background(), g.c, g.models, g.dir, time.Minute)
			err = restored.recoverTasks()
			if kind == "malformed" {
				if err == nil {
					t.Fatal("malformed index was accepted")
				}
			} else if err != nil || len(restored.threads) != 0 {
				t.Fatal("older index revived a branch already extended by a task", err)
			}
		})
	}
}

func TestDurableThreadRecoveryFiltersScopeExpiryAndChangedHistory(t *testing.T) {
	for _, kind := range []string{"account", "device", "expired", "edited", "model", "tools"} {
		t.Run(kind, func(t *testing.T) {
			g, next, _ := completedThread(t)
			switch kind {
			case "account":
				g.c.session.AccountID = "another-account"
			case "device":
				g.c.session.DeviceID = "another-device"
			case "expired":
				for _, th := range g.threads {
					th.used = time.Now().Add(-threadTTL - time.Minute)
				}
				if err := g.saveThreadsLocked(); err != nil {
					t.Fatal(err)
				}
			case "edited":
				next.Messages[0].Content = "edited earlier message"
			case "model":
				next.Model = "another-model"
			case "tools":
				next.Tools = toolRequest().Tools
			}
			restored, _, starts := restartThreadGateway(t, g)
			if _, failure := restored.submit(next, "changed-context"); failure != nil {
				t.Fatal(failure)
			}
			if got := <-starts; got.conversation != "" {
				t.Fatal("incompatible history reused native context", got)
			}
		})
	}
}

func TestDurableToolThreadPinsWorkspaceAfterTasksAreSwept(t *testing.T) {
	for _, kind := range []string{"retain", "missing", "routing-changed"} {
		t.Run(kind, func(t *testing.T) {
			g, server := testGateway(t)
			folder := filepath.Join(g.dir, "gateway-workspace-"+token())
			if err := os.Mkdir(folder, 0700); err != nil {
				t.Fatal(err)
			}
			req := toolRequest()
			g.run = func(turn *gatewayTurn) {
				g.mu.Lock()
				turn.conversation, turn.mode = "tool-conversation", "tools"
				turn.workspace, turn.sandbox = folder, "original-sandbox"
				turn.generationPosts++
				g.mu.Unlock()
				g.finish(turn, "tool answer", nil)
			}
			status, response := postGateway(t, server, req, "tool-fixture")
			if status != 200 {
				t.Fatal(status, response)
			}
			g.wg.Wait()
			next := req
			next.Messages = append(append([]apiMessage{}, req.Messages...), returnedMessage(t, response), apiMessage{Role: "user", Content: "continue"})
			g.mu.Lock()
			for _, turn := range g.turns {
				g.dropTurnLocked(turn)
			}
			g.mu.Unlock()
			if kind == "missing" {
				if err := os.Remove(folder); err != nil {
					t.Fatal(err)
				}
			} else if kind == "routing-changed" {
				g.nativeTools = !g.nativeTools
			}
			restored, _, _ := restartThreadGateway(t, g)
			if kind == "retain" {
				if len(restored.threads) != 1 {
					t.Fatal("unclaimed tool head was lost")
				}
				if _, err := os.Stat(folder); err != nil {
					t.Fatal("orphan sweep removed pinned workspace", err)
				}
				// There is no source task left to pin the folder during claim.
				starts := make(chan *gatewayTurn, 1)
				restored.run = func(turn *gatewayTurn) { starts <- turn }
				if _, failure := restored.submit(next, "pin-handoff"); failure != nil {
					t.Fatal(failure)
				}
				turn := <-starts
				defer turn.cancel()
				defer turn.timer.Stop()
				if turn.workspace != folder || turn.sandbox != "original-sandbox" {
					t.Fatal("native workspace/sandbox changed on claim")
				}
				if _, err := os.Stat(folder); err != nil {
					t.Fatal("claim removed original workspace", err)
				}
			} else if len(restored.threads) != 0 {
				t.Fatal("unusable local-tool head was restored")
			}
		})
	}
}
