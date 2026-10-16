package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type ServiceConfig struct {
	StateDir, HelperPath string
	Transport            http.RoundTripper
	TaskTimeout          time.Duration
}
type PublicModel struct{ ID, DisplayName string }
type Service struct {
	c      *Client
	g      *gateway
	cancel context.CancelFunc
	Models []PublicModel
	lock   *os.File
}

func OpenService(ctx context.Context, config ServiceConfig) (*Service, error) {
	if !filepath.IsAbs(config.StateDir) || !filepath.IsAbs(config.HelperPath) {
		return nil, errors.New("state_dir and helper_path must be absolute paths")
	}
	if _, err := os.Stat(config.HelperPath); err != nil {
		return nil, errors.New("doubao helper is not installed")
	}
	if err := os.MkdirAll(config.StateDir, 0700); err != nil {
		return nil, err
	}
	lock, err := lockFile(config.StateDir, "provider.lock", true)
	if err != nil {
		return nil, errors.New("state_dir is already owned by another provider instance")
	}
	opened := false
	defer func() {
		if !opened {
			unlockFile(lock)
		}
	}()
	c, err := newClient(config.StateDir)
	if err != nil {
		return nil, err
	}
	if config.Transport != nil {
		c.http.Transport = config.Transport
		c.mediaHTTP = &http.Client{Transport: config.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	credential, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = c.account(credential); err != nil {
		return nil, apiFail(401, "login_required", "Doubao login expired; sign in again")
	}
	models, err := c.models(credential)
	if err != nil {
		return nil, err
	}
	aliases := []Model{}
	public := []PublicModel{}
	for _, m := range models {
		if m.Available && contains(m.Modes, "work") {
			alias := "doubao-work-" + m.ID
			public = append(public, PublicModel{alias, m.Name})
			m.Name = alias
			aliases = append(aliases, m)
		}
	}
	for _, m := range models {
		if m.Available && contains(m.Modes, "work") && m.Name == "豆包 2.1 Pro" {
			m.Name = "doubao-work-pro"
			aliases = append(aliases, m)
			public = append(public, PublicModel{m.Name, "豆包 2.1 Pro"})
			break
		}
	}
	if len(aliases) == 0 {
		return nil, errors.New("no available Work models")
	}
	lifetime, stop := context.WithCancel(context.Background())
	if config.TaskTimeout == 0 {
		config.TaskTimeout = 10 * time.Minute
	}
	g := newGateway(lifetime, c, aliases, config.StateDir, config.TaskTimeout)
	g.apiKey = ""
	g.helperPath = config.HelperPath
	s := &Service{c: c, g: g, cancel: stop, Models: public, lock: lock}
	if err = g.recoverTasks(); err != nil {
		stop()
		return nil, err
	}
	opened = true
	return s, nil
}
func (s *Service) Validate(ctx context.Context) error {
	if err := s.c.account(ctx); err != nil {
		return apiFail(401, "login_required", "Doubao login expired; sign in again")
	}
	return nil
}
func (s *Service) Close() {
	s.g.mu.Lock()
	s.g.suspending = true
	s.g.mu.Unlock()
	s.cancel()
	s.g.wg.Wait()
	unlockFile(s.lock)
}
func (e *apiError) StatusCode() int { return e.Status }

type Response struct{ x *apiExchange }

func (s *Service) Submit(ctx context.Context, payload []byte, key string) (*Response, error) {
	req, failure := parseAPIRequest(bytes.NewReader(payload))
	if failure != nil {
		return nil, failure
	}
	if _, _, err := selectModel(s.g.models, req.Model, "work"); err != nil {
		return nil, apiFail(400, "model_not_found", "Select an available doubao-work model")
	}
	x, failure := s.g.submit(req, key)
	if failure != nil {
		return nil, failure
	}
	return &Response{x}, nil
}
func (r *Response) Complete(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.x.ready:
	}
	if r.x.err != nil {
		return nil, r.x.err
	}
	return json.Marshal(apiCompletion(r.x))
}
func (r *Response) Stream(ctx context.Context, emit func([]byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &emitWriter{headers: http.Header{}, emit: func(data []byte) error {
		err := emit(data)
		if err != nil {
			cancel()
		}
		return err
	}}
	request, _ := http.NewRequestWithContext(ctx, "POST", "http://plugin/v1/chat/completions", nil)
	writeAPIExchange(w, request, r.x, true)
	return w.err
}

type emitWriter struct {
	headers http.Header
	emit    func([]byte) error
	err     error
}

func (w *emitWriter) Header() http.Header { return w.headers }
func (w *emitWriter) WriteHeader(int)     {}
func (w *emitWriter) Flush()              {}
func (w *emitWriter) Write(p []byte) (int, error) {
	if w.err == nil {
		w.err = w.emit(p)
	}
	return len(p), w.err
}

func (s *Service) HasActiveTasks() bool {
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	for _, t := range s.g.turns {
		if !t.done {
			return true
		}
	}
	return false
}
