package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type ServiceConfig struct {
	StateDir, HelperPath string
	Transport            http.RoundTripper
	TaskTimeout          time.Duration
	ConnectorReuse       bool
	NativeTools          bool
}

// PublicModel is a routable model: ID is its slug, DisplayName the catalog
// name, Window the advertised context and Efforts Work's reasoning levels.
type PublicModel struct {
	ID, DisplayName string
	Window          int
	Efforts         []int
}

// MaxPromptChars is Work's per-request message limit in UTF-16 code units.
const MaxPromptChars = maxPromptChars

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
	c.deferToNewer = true
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
	aliases := publicModels(models)
	public := []PublicModel{}
	for _, m := range aliases {
		public = append(public, PublicModel{ID: m.Name, DisplayName: m.Display, Window: m.Window, Efforts: m.Efforts})
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
	g.nativeTools = config.NativeTools
	g.helperPath = config.HelperPath
	if config.ConnectorReuse {
		g.connectors, err = loadConnectorPool(g)
		if err != nil {
			stop()
			return nil, err
		}
	}
	s := &Service{c: c, g: g, cancel: stop, Models: public, lock: lock}
	if err = g.recoverTasks(); err != nil {
		stop()
		return nil, err
	}
	if g.connectors != nil {
		g.connectors.start()
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

// publicModels keeps the Work models and names each by its slug. CPA routes by
// name across providers, so every slug carries the doubao- prefix.
func publicModels(models []Model) []Model {
	public, seen := []Model{}, map[string]bool{}
	for _, m := range models {
		if !m.Available || !contains(m.Modes, "work") {
			continue
		}
		slug := modelSlug(m)
		if seen[slug] {
			slug += "-" + m.ID
		}
		seen[slug] = true
		m.Name = slug
		public = append(public, m)
	}
	return public
}

var slugSeparator = regexp.MustCompile(`[^a-z0-9.]+`)

// modelSlug turns a catalog name such as "豆包 2.1 Pro" or "GPT-6.1 Sol" into
// doubao-2.1-pro or doubao-gpt-6.1-sol.
func modelSlug(m Model) string {
	if m.Name == "自动" {
		return "doubao-auto"
	}
	slug := strings.Trim(slugSeparator.ReplaceAllString(strings.ToLower(strings.ReplaceAll(m.Name, "豆包", "")), "-"), "-.")
	if slug == "" {
		slug = m.ID
	}
	return "doubao-" + slug
}

// SessionReplaced reports that another login rewrote session.json; the
// service keeps stale cookies until it is reopened.
func (s *Service) SessionReplaced() bool {
	s.c.saveMu.Lock()
	defer s.c.saveMu.Unlock()
	return s.c.replaced(filepath.Join(s.c.dir, "session.json"))
}
func (s *Service) Close() {
	s.g.mu.Lock()
	s.g.suspending = true
	s.g.mu.Unlock()
	s.cancel()
	s.g.wg.Wait()
	if s.g.connectors != nil {
		s.g.connectors.close()
	}
	unlockFile(s.lock)
}
func (e *apiError) StatusCode() int   { return e.Status }
func (e *apiError) ErrorCode() string { return e.Code }
func (e *apiError) ErrorType() string { return e.Type }

type Response struct {
	x *apiExchange
	// usage asks for the separate usage chunk of stream_options.include_usage.
	usage bool
}

func (s *Service) Submit(ctx context.Context, payload []byte, key string) (*Response, error) {
	req, failure := parseAPIRequest(bytes.NewReader(payload))
	if failure != nil {
		return nil, failure
	}
	if _, _, err := selectModel(s.g.models, req.Model, "work"); err != nil {
		// Not model_not_found: CPA would bench the model for 12 hours, past the
		// next catalog reload that may bring it back.
		return nil, apiFail(400, "invalid_model", "Select a model listed by /v1/models")
	}
	x, failure := s.g.submit(req, key)
	if failure != nil {
		return nil, failure
	}
	return &Response{x: x, usage: truthy(req.StreamOptions["include_usage"])}, nil
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

// Stream emits SSE frames and returns the upstream failure, if any, so the
// host can close the stream with an error instead of a silent end.
func (r *Response) Stream(ctx context.Context, emit func([]byte) error) error {
	return streamExchange(ctx, func(frame string) error { return emit([]byte(frame)) }, r.x, r.usage)
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
