package doubao

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Only the current, unclaimed conversation heads are durable. Reconstructing
// heads from completed task receipts would resurrect already consumed branches.
// Histories are hashes; the index contains no messages, tool results or cookies.
type savedGatewayThread struct {
	Conversation string    `json:"conversation"`
	Mode         string    `json:"mode"`
	Workspace    string    `json:"workspace,omitempty"`
	Sandbox      string    `json:"sandbox,omitempty"`
	Context      int       `json:"context_tokens,omitempty"`
	Used         time.Time `json:"used"`
	Header       string    `json:"header"`
	Keys         []string  `json:"keys"`
	NativeTools  bool      `json:"native_tools"`
}

type savedGatewayThreads struct {
	Version int                           `json:"version"`
	Scope   string                        `json:"scope"`
	Heads   map[string]savedGatewayThread `json:"heads"`
}

func (g *gateway) threadsPath() string { return filepath.Join(g.dir, "gateway-threads.json") }
func (g *gateway) threadsScope() string {
	return apiHash([]string{g.c.session.AccountID, g.c.session.DeviceID})
}

func (g *gateway) saveThreadsLocked() error {
	saved := savedGatewayThreads{Version: 1, Scope: g.threadsScope(), Heads: map[string]savedGatewayThread{}}
	for key, th := range g.threads {
		saved.Heads[key] = savedGatewayThread{Conversation: th.conversation, Mode: th.mode,
			Workspace: th.workspace, Sandbox: th.sandbox, Context: th.context, Used: th.used,
			Header: th.header, Keys: th.keys, NativeTools: th.nativeTools}
	}
	return saveJSON(g.threadsPath(), saved)
}

func validThreadHash(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// recoverThreads runs after task checkpoints were read, before any recovered
// worker starts or orphaned workspaces are reclaimed. A missing index starts
// empty, including on upgrades; task receipts never seed continuation heads.
func (g *gateway) recoverThreads() error {
	data, err := os.ReadFile(g.threadsPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	saved := savedGatewayThreads{Version: 1, Scope: g.threadsScope(), Heads: map[string]savedGatewayThread{}}
	if err == nil && (json.Unmarshal(data, &saved) != nil || saved.Version != 1 ||
		!validThreadHash(saved.Scope) || saved.Heads == nil || len(saved.Heads) > threadLimit) {
		return errors.New("invalid conversation continuation index")
	}
	heads := map[string]*gatewayThread{}
	conversations := map[string]bool{}
	if saved.Scope == g.threadsScope() {
		for key, th := range saved.Heads {
			if !validThreadHash(key) || !validThreadHash(th.Header) || len(th.Keys) == 0 ||
				key != threadKey(th.Header, th.Keys[len(th.Keys)-1]) || th.Conversation == "" ||
				(th.Mode != "work" && th.Mode != "tools") || th.Used.IsZero() || th.Context < 0 ||
				th.Mode == "work" && (th.Workspace != "" || th.Sandbox != "") {
				return errors.New("invalid conversation continuation head")
			}
			for _, hash := range th.Keys {
				if !validThreadHash(hash) {
					return errors.New("invalid conversation history hash")
				}
			}
			if conversations[th.Conversation] {
				return errors.New("duplicate conversation continuation head")
			}
			conversations[th.Conversation] = true
			if time.Since(th.Used) > threadTTL || th.Used.After(time.Now()) ||
				th.Mode == "tools" && th.NativeTools != g.nativeTools {
				continue
			}
			if th.Mode == "tools" && !g.threadWorkspaceExists(th.Workspace, th.Sandbox) {
				continue
			}
			claimed := false
			for _, turn := range g.turns {
				if turn.conversation != th.Conversation {
					continue
				}
				// Also reject a stale head if a retained checkpoint already
				// extends it, even when that newer task has ended in error.
				keys := threadKeys(turn.request.Messages)
				if !turn.done || len(keys) >= len(th.Keys) && keys[len(th.Keys)-1] == th.Keys[len(th.Keys)-1] {
					claimed = true
					break
				}
			}
			if !claimed {
				heads[key] = &gatewayThread{conversation: th.Conversation, mode: th.Mode,
					workspace: th.Workspace, sandbox: th.Sandbox, context: th.Context,
					used: th.Used, header: th.Header, keys: th.Keys, nativeTools: th.NativeTools}
			}
		}
	}
	g.threads = heads
	return g.saveThreadsLocked()
}

// Work binds local-tool conversations to their original sandbox and folder.
// Never silently recreate a missing folder or accept one outside this state.
func (g *gateway) threadWorkspaceExists(workspace, sandbox string) bool {
	if sandbox == "" || !filepath.IsAbs(workspace) || filepath.Dir(workspace) != filepath.Clean(g.dir) ||
		!strings.HasPrefix(filepath.Base(workspace), "gateway-workspace-") {
		return false
	}
	info, err := os.Lstat(workspace)
	return err == nil && info.IsDir()
}
