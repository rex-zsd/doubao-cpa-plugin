package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"doubao-cpa-plugin/internal/doubao"
)

// The plugin page is a CPA resource route, which CPA serves without
// authentication, so the page itself holds no data. Everything it shows or
// does goes through the management routes below, which require the CPA
// management key. They expose metadata only: task states, counts and timings,
// never prompts, tool arguments, results or the login session.

//go:embed console.html
var consoleHTML []byte

const managementBase = "/v0/management/doubao-work"

var managementRoutes = []struct{ method, path string }{
	{"GET", "/status"},
	{"GET", "/tasks"},
	{"POST", "/tasks/cancel"},
	{"POST", "/login/start"},
	{"GET", "/login/status"},
	{"POST", "/test"},
	{"POST", "/models/refresh"},
	{"GET", "/cleanup"},
	{"POST", "/cleanup/conversations"},
	{"POST", "/cleanup/connectors"},
	{"POST", "/cleanup/files"},
}

func managementRegistration() object {
	routes := []any{}
	for _, r := range managementRoutes {
		routes = append(routes, object{"Method": r.method, "Path": managementBase + r.path})
	}
	return object{"routes": routes, "resources": []any{object{"Path": "/console", "Menu": "豆包 Work", "Description": "登录、运行状态、任务与测试"}}}
}

type managementRequest struct {
	Method     string
	Path       string
	Query      url.Values
	Body       []byte
	CallbackID string `json:"host_callback_id"`
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func managementJSON(status int, value any) managementResponse {
	body, _ := json.Marshal(value)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: body}
}

func managementError(status int, message string) managementResponse {
	return managementJSON(status, object{"error": message})
}

func handleManagement(raw []byte) managementResponse {
	var req managementRequest
	if json.Unmarshal(raw, &req) != nil {
		return managementError(400, "invalid management request")
	}
	if strings.HasPrefix(req.Path, "/v0/resource/plugins/") && strings.HasSuffix(req.Path, "/console") {
		return managementResponse{StatusCode: 200, Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
		}, Body: consoleHTML}
	}
	route := strings.ToUpper(req.Method) + " " + strings.TrimPrefix(req.Path, managementBase)
	switch route {
	case "GET /status":
		return managementJSON(200, consoleStatus())
	case "GET /tasks":
		s, err := consoleService()
		if err != nil {
			return managementJSON(200, object{"tasks": []any{}})
		}
		return managementJSON(200, object{"tasks": s.Tasks()})
	case "POST /tasks/cancel":
		var body struct{ ID string }
		if json.Unmarshal(req.Body, &body) != nil || body.ID == "" {
			return managementError(400, "missing task id")
		}
		s, err := consoleService()
		if err != nil || !s.CancelTask(body.ID) {
			return managementError(404, "no running task with this id")
		}
		return managementJSON(200, object{"cancelled": true})
	case "POST /login/start":
		return consoleLoginStart()
	case "GET /login/status":
		result, err := pollLogin(req.Query.Get("state"))
		if err != nil {
			return managementError(errorStatus(err), err.Error())
		}
		return managementJSON(200, object{"status": result["Status"], "message": result["Message"]})
	case "POST /test":
		return consoleTest(req.Body)
	case "POST /models/refresh":
		return consoleRefreshModels(req.CallbackID)
	case "GET /cleanup":
		return consoleCleanupSummary()
	case "POST /cleanup/conversations":
		var body struct{ Days int }
		if json.Unmarshal(req.Body, &body) != nil || body.Days < 0 || body.Days > 3650 {
			return managementError(400, "days must be between 0 and 3650")
		}
		return consoleCleanup(func(ctx context.Context, s *doubao.Service) (object, error) {
			deleted, remaining, err := s.DeleteConversations(ctx, time.Duration(body.Days)*24*time.Hour)
			return object{"deleted": deleted, "remaining": remaining}, err
		})
	case "POST /cleanup/connectors":
		return consoleCleanup(func(ctx context.Context, s *doubao.Service) (object, error) {
			removed, err := s.CleanupConnectors(ctx)
			return object{"removed": removed}, err
		})
	case "POST /cleanup/files":
		return consoleCleanup(func(_ context.Context, s *doubao.Service) (object, error) {
			deleted, err := s.DeleteFiles()
			return object{"deleted": deleted}, err
		})
	}
	return managementError(404, "unknown plugin route")
}

// consoleService returns the open service without opening one: the page
// must not load the account or the model list just by being viewed.
func consoleService() (*doubao.Service, error) {
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.service == nil {
		return nil, errors.New("service not open")
	}
	return runtime.service, nil
}

var loginCheck struct {
	at  time.Time
	err error
}

// noteLoginCheck records the latest account validation, from CPA's periodic
// auth refresh or from the page.
func noteLoginCheck(err error) {
	runtime.Lock()
	loginCheck.at, loginCheck.err = time.Now(), err
	runtime.Unlock()
}

func consoleStatus() object {
	runtime.Lock()
	cfg, flow, open, settingsPending := runtime.config, runtime.login, runtime.service, runtime.pending != nil
	checkedAt, checkErr := loginCheck.at, loginCheck.err
	runtime.Unlock()
	status := object{"version": pluginVersion, "native_tools": cfg.NativeTools, "service_open": open != nil, "settings_pending": settingsPending}
	login := object{}
	pending := false
	if flow != nil {
		select {
		case <-flow.done:
		default:
			pending = true
		}
	}
	switch {
	case pending:
		login["state"] = "pending"
	default:
		s, err := openService()
		if err != nil {
			login["state"], login["message"] = "unknown", err.Error()
			if errorStatus(err) == 401 {
				login["state"] = "expired"
			}
			break
		}
		// One account check a minute at most, shared with CPA's refresh.
		if time.Since(checkedAt) > time.Minute {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			checkErr = s.Validate(ctx)
			cancel()
			noteLoginCheck(checkErr)
			checkedAt = time.Now()
		}
		login["state"], login["checked_at"] = "ok", checkedAt
		if checkErr != nil {
			login["state"] = "expired"
		}
		status["stats"] = s.Stats()
		models := []any{}
		for _, m := range s.PublicModels() {
			models = append(models, object{"id": m.ID, "name": m.DisplayName})
		}
		status["models"], status["models_refreshed_at"] = models, s.ModelsRefreshed()
		status["service_open"] = true
	}
	status["login"] = login
	return status
}

func consoleLoginStart() managementResponse {
	result, err := startLogin()
	if err != nil {
		return managementError(errorStatus(err), err.Error())
	}
	runtime.Lock()
	stateDir := runtime.config.StateDir
	runtime.Unlock()
	response := object{"state": result["State"], "expires_at": result["ExpiresAt"]}
	if path, _ := field(result, "Metadata", "qr_path").(string); path != "" {
		path = filepath.Clean(path)
		if rel, err := filepath.Rel(stateDir, path); err == nil && !strings.HasPrefix(rel, "..") {
			if info, err := os.Stat(path); err == nil && info.Size() < 1<<20 {
				if png, err := os.ReadFile(path); err == nil {
					response["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
				}
			}
		}
	}
	return managementJSON(200, response)
}

func field(value any, path ...string) any {
	for _, key := range path {
		m, ok := value.(object)
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

// consoleTest sends one short question through the same path as an API
// request and times the first visible text and the full answer.
func consoleTest(raw []byte) managementResponse {
	var body struct{ Model, Prompt string }
	if json.Unmarshal(raw, &body) != nil || body.Model == "" || strings.TrimSpace(body.Prompt) == "" || utf8.RuneCountInString(body.Prompt) > 2000 {
		return managementError(400, "choose a model and a prompt of at most 2000 characters")
	}
	s, err := openService()
	if err != nil {
		return managementError(errorStatus(err), err.Error())
	}
	payload, _ := json.Marshal(object{"model": body.Model, "messages": []any{object{"role": "user", "content": body.Prompt}}, "stream": true})
	runtime.Lock()
	parent := runtime.ctx
	runtime.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	started := time.Now()
	response, err := s.Submit(ctx, payload, "")
	if err != nil {
		return managementJSON(200, object{"ok": false, "error": err.Error(), "total_ms": time.Since(started).Milliseconds()})
	}
	var reply strings.Builder
	firstText := int64(0)
	err = response.Stream(ctx, func(frame []byte) error {
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(frame)), "data:"))
		var chunk struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return nil
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" && firstText == 0 {
				firstText = time.Since(started).Milliseconds()
			}
			reply.WriteString(choice.Delta.Content)
		}
		return nil
	})
	result := object{"ok": err == nil, "first_text_ms": firstText, "total_ms": time.Since(started).Milliseconds(), "reply": reply.String()}
	if err != nil {
		result["error"] = err.Error()
	}
	return managementJSON(200, result)
}

// consoleRefreshModels reloads Work's model list, then rewrites the plugin's
// auth file with a refresh mark so that CPA reloads the account and asks the
// plugin for its models again.
func consoleRefreshModels(callbackID string) managementResponse {
	s, err := openService()
	if err != nil {
		return managementError(errorStatus(err), err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	added, removed, err := s.RefreshModels(ctx)
	if err != nil {
		return managementError(502, err.Error())
	}
	result := object{"added": added, "removed": removed, "total": len(s.PublicModels()), "cpa_notified": false}
	if err := notifyModelChange(callbackID); err != nil {
		result["cpa_error"] = err.Error()
	} else {
		result["cpa_notified"] = true
	}
	return managementJSON(200, result)
}

func notifyModelChange(callbackID string) error {
	var list struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Provider  string `json:"provider"`
		} `json:"files"`
	}
	if err := hostCall("host.auth.list", object{"host_callback_id": callbackID}, &list); err != nil {
		return err
	}
	for _, file := range list.Files {
		if file.Provider != provider && file.Name != "doubao-work.json" {
			continue
		}
		var current struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := hostCall("host.auth.get", object{"auth_index": file.AuthIndex, "host_callback_id": callbackID}, &current); err != nil {
			return err
		}
		var record object
		if json.Unmarshal(current.JSON, &record) != nil || record["type"] != provider {
			return errors.New("plugin auth file is not a Doubao Work credential")
		}
		record["models_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)
		data, _ := json.Marshal(record)
		name := current.Name
		if name == "" {
			name = file.Name
		}
		return hostCall("host.auth.save", object{"name": filepath.Base(name), "json": json.RawMessage(data), "host_callback_id": callbackID}, nil)
	}
	return errors.New("no Doubao Work credential is registered in CPA")
}

func consoleCleanupSummary() managementResponse {
	s, err := consoleService()
	if err != nil {
		return managementError(409, "the plugin has not loaded its account yet; open the status first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := object{"conversations": s.Conversations()}
	if connectors, err := s.Connectors(ctx); err == nil {
		result["connectors"] = connectors
	} else {
		result["connectors_error"] = err.Error()
	}
	if files, err := s.Files(); err == nil {
		result["files"] = files
	} else {
		result["files_error"] = err.Error()
	}
	return managementJSON(200, result)
}

func consoleCleanup(run func(context.Context, *doubao.Service) (object, error)) managementResponse {
	s, err := consoleService()
	if err != nil {
		return managementError(409, "the plugin has not loaded its account yet; open the status first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := run(ctx, s)
	if err != nil {
		if result == nil {
			result = object{}
		}
		result["error"] = err.Error()
		return managementJSON(errorStatus(err), result)
	}
	return managementJSON(200, result)
}
