package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type loginFlow struct {
	id, dir string
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func startLogin() (object, error) {
	runtime.Lock()
	if runtime.closed {
		runtime.Unlock()
		return nil, &statusError{503, "Plugin is shutting down"}
	}
	if flow := runtime.login; flow != nil {
		select {
		case <-flow.done:
		default:
			runtime.Unlock()
			return loginReady(flow)
		}
	}
	if runtime.service != nil {
		if runtime.service.HasActiveTasks() {
			runtime.Unlock()
			return nil, &statusError{409, "Complete or cancel active tasks before starting another login"}
		}
		runtime.service.Close()
		runtime.service = nil
	}
	cfg := runtime.config
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		runtime.Unlock()
		return nil, err
	}
	status := filepath.Join(cfg.StateDir, "login-status.json")
	_ = os.Remove(status)
	log, err := os.OpenFile(filepath.Join(cfg.StateDir, "login.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		runtime.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		cancel()
		log.Close()
		runtime.Unlock()
		return nil, err
	}
	flow := &loginFlow{id: hex.EncodeToString(key), dir: cfg.StateDir, cancel: cancel, done: make(chan struct{})}
	cmd := exec.CommandContext(ctx, cfg.HelperPath, "-action", "login", "-dir", cfg.StateDir, "-out", filepath.Join(cfg.StateDir, "login-evidence"), "-timeout", "5m")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		cancel()
		log.Close()
		runtime.Unlock()
		return nil, err
	}
	runtime.login = flow
	go func() {
		err := cmd.Wait()
		log.Close()
		flow.mu.Lock()
		flow.err = err
		flow.mu.Unlock()
		cancel()
		close(flow.done)
	}()
	runtime.Unlock()
	return loginReady(flow)
}
func (f *loginFlow) readStatus() (object, error) {
	data, err := os.ReadFile(filepath.Join(f.dir, "login-status.json"))
	if err != nil {
		return nil, err
	}
	var state object
	if json.Unmarshal(data, &state) != nil {
		return nil, errors.New("invalid login state")
	}
	return state, nil
}
func loginReady(flow *loginFlow) (object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := flow.readStatus()
		if err == nil && state["url"] != nil {
			return object{"Provider": provider, "URL": state["url"], "State": flow.id, "Metadata": object{"qr_path": state["qr_path"]}, "ExpiresAt": state["expires_at"]}, nil
		}
		select {
		case <-flow.done:
			return nil, errors.New("QR login helper failed; inspect private login log")
		case <-ctx.Done():
			return nil, errors.New("QR login did not become ready")
		case <-ticker.C:
		}
	}
}
func pollLogin(id string) (object, error) {
	runtime.Lock()
	flow := runtime.login
	runtime.Unlock()
	if flow == nil || id != flow.id {
		return nil, &statusError{400, "Unknown or expired login flow"}
	}
	status, readErr := flow.readStatus()
	if readErr == nil && status["status"] == "verified" {
		return object{"Status": "success", "Auth": authRecord("doubao-work.json")}, nil
	}
	select {
	case <-flow.done:
		return object{"Status": "error", "Message": "Login failed or expired; start a new QR flow"}, nil
	default:
	}
	if readErr != nil {
		return object{"Status": "pending", "Message": "Waiting for QR code"}, nil
	}
	if expiry, err := time.Parse(time.RFC3339, textString(status["expires_at"])); err == nil && !expiry.After(time.Now()) {
		flow.cancel()
		return object{"Status": "error", "Message": "QR code expired; start a new login"}, nil
	}
	return object{"Status": "pending", "Message": status["status"]}, nil
}
func textString(value any) string { text, _ := value.(string); return text }
