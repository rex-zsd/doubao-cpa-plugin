package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doubao-cpa-plugin/internal/doubao"
	"gopkg.in/yaml.v3"
)

const provider = "doubao-work"

type object = map[string]any
type config struct {
	StateDir    string `yaml:"state_dir"`
	HelperPath  string `yaml:"helper_path"`
	NativeTools bool   `yaml:"native_tools"`
}

var runtime struct {
	sync.Mutex
	config  config
	service *doubao.Service
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	streams sync.WaitGroup
	login   *loginFlow
}

func failure(code, message string, status int) []byte {
	b, _ := json.Marshal(object{"ok": false, "error": object{"code": code, "message": message, "http_status": status, "retryable": false}})
	return b
}
func failureFrom(err error) []byte {
	status := errorStatus(err)
	var typed typedError
	if errors.As(err, &typed) {
		return failure(typed.ErrorCode(), errorJSON(typed, status), status)
	}
	return failure("doubao_error", err.Error(), status)
}

// typedError is doubao's API error. CPA passes a JSON message through to
// OpenAI clients and reads type and message from it for Claude clients, so a
// typed error travels as the JSON body a client expects.
type typedError interface {
	error
	ErrorCode() string
	ErrorType() string
}

func errorStatus(err error) int {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		return coded.StatusCode()
	}
	return 500
}

// errorType names a failure in terms both OpenAI and Anthropic clients act
// on; Anthropic has no server_error.
func errorType(status int, fallback string) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	}
	return fallback
}
func errorJSON(err typedError, status int) string {
	b, _ := json.Marshal(object{"error": object{"message": err.Error(), "type": errorType(status, err.ErrorType()), "code": err.ErrorCode()}})
	return string(b)
}

// streamFailure is the error a stream closes with. CPA keeps only its text,
// so a typed error is sent as JSON: clients then still see, for example, the
// context_length_exceeded that makes them compact.
func streamFailure(err error) string {
	var typed typedError
	if errors.As(err, &typed) {
		return errorJSON(typed, errorStatus(err))
	}
	return "Doubao stream interrupted"
}

type request struct {
	AuthID, AuthProvider, Provider, Model, Format, SourceFormat, State string
	Path, FileName                                                     string
	Payload, OriginalRequest, StorageJSON, RawJSON                     []byte
	Headers                                                            http.Header
	Metadata                                                           map[string]any
	Stream                                                             bool
	StreamID                                                           string `json:"stream_id"`
	CallbackID                                                         string `json:"host_callback_id"`
}

// CPA resolves model aliases in Model. When no format translation is needed,
// Payload still contains the client-facing alias, so use the host's resolved
// model for execution. Raw fields preserve tool arguments and large numbers.
func executorPayload(req request) ([]byte, error) {
	payload := req.Payload
	var err error
	if req.SourceFormat == "openai-response" {
		payload, err = doubao.NormalizeResponses(payload)
		if err != nil {
			return nil, err
		}
	}
	if claudeClientRequest(req) && len(req.OriginalRequest) > 0 {
		payload, err = restoreClaudeToolErrors(payload, req.OriginalRequest)
		if err != nil {
			return nil, err
		}
		payload, err = restoreClaudeParallel(payload, req.OriginalRequest)
		if err != nil {
			return nil, err
		}
	}
	if req.Model == "" {
		return payload, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		// Keep malformed input for the existing API validator to report as 400.
		return payload, nil
	}
	model, _ := json.Marshal(req.Model)
	if bytes.Equal(bytes.TrimSpace(fields["model"]), model) {
		return payload, nil
	}
	fields["model"] = model
	return json.Marshal(fields)
}

func openService() (*doubao.Service, error) {
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.closed {
		return nil, errors.New("plugin is shutting down")
	}
	if s := runtime.service; s != nil {
		// Helper login replaced the saved session: reopen once idle so the
		// newer login is used instead of stale in-memory cookies.
		if !s.SessionReplaced() || s.HasActiveTasks() {
			return s, nil
		}
		s.Close()
		runtime.service = nil
	}
	if runtime.login != nil {
		select {
		case <-runtime.login.done:
		default:
			return nil, &statusError{401, "Complete the current QR login before loading models"}
		}
	}
	s, err := doubao.OpenService(context.Background(), serviceConfig(runtime.config))
	if err == nil {
		runtime.service = s
	}
	return s, err
}

// serviceConfig keeps upstream HTTP inside the plugin. The native clients
// refuse redirects and pin public media downloads, so the official CPA host
// needs no HTTP bridge.
func serviceConfig(cfg config) doubao.ServiceConfig {
	return doubao.ServiceConfig{StateDir: cfg.StateDir, HelperPath: cfg.HelperPath, ConnectorReuse: true, NativeTools: cfg.NativeTools}
}
func closePlugin() {
	runtime.Lock()
	runtime.closed = true
	s := runtime.service
	login := runtime.login
	if runtime.cancel != nil {
		runtime.cancel()
	}
	runtime.service = nil
	runtime.Unlock()
	if login != nil {
		login.cancel()
		<-login.done
	}
	if s != nil {
		s.Close()
	}
	runtime.streams.Wait()
}

// authRecord turns off CPA cooldown for the account. One Doubao login serves
// every model, so there is no other credential to fail over to and a cooldown
// would only refuse requests the gateway could take once a task finishes.
func authRecord(id string) object {
	storage := []byte(`{"type":"doubao-work","version":1}`)
	return object{"Provider": provider, "ID": id, "FileName": "doubao-work.json", "Label": "Doubao Work", "StorageJSON": storage, "Metadata": object{"type": provider, "disable_cooling": true}, "Attributes": map[string]string{}}
}

// effortLevels names Work's reasoning levels by the reasoning_effort values
// that select them.
var effortLevels = map[int]string{3: "low", 4: "medium", 5: "high", 6: "xhigh", 7: "max"}

// modelInfo describes a model for CPA's listings. Work takes one message of at
// most doubao.MaxPromptChars characters, which no prompt of 65,536 estimated
// tokens exceeds, so that is the context advertised unless the model's own
// window is smaller.
func modelInfo(m doubao.PublicModel) object {
	window := 65536
	if m.Window > 0 && m.Window < window {
		window = m.Window
	}
	info := object{"ID": m.ID, "Object": "model", "OwnedBy": provider, "Type": provider, "DisplayName": m.DisplayName, "Name": m.ID, "ContextLength": window, "Description": fmt.Sprintf("%s on Doubao Work; each request is sent as one message of at most %d characters", m.DisplayName, doubao.MaxPromptChars), "SupportedInputModalities": []string{"text", "image", "file"}, "SupportedOutputModalities": []string{"text"}, "SupportedParameters": []string{"tools", "tool_choice", "parallel_tool_calls", "reasoning_effort", "response_format", "stream_options"}}
	levels := []string{}
	for _, effort := range m.Efforts {
		if level, ok := effortLevels[effort]; ok {
			levels = append(levels, level)
		}
	}
	if len(levels) > 0 {
		info["Thinking"] = object{"Levels": levels}
	}
	return info
}
func dispatch(method string, raw []byte) (any, error) {
	var req request
	if len(raw) > 0 && json.Unmarshal(raw, &req) != nil {
		return nil, fmt.Errorf("invalid plugin request")
	}
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var incoming struct {
			Config []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &incoming); err != nil {
			return nil, err
		}
		var cfg config
		if err := yaml.Unmarshal(incoming.Config, &cfg); err != nil {
			return nil, errors.New("invalid plugin YAML")
		}
		if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.HelperPath) {
			return nil, errors.New("configure absolute state_dir and helper_path")
		}
		runtime.Lock()
		changed := runtime.service != nil && runtime.config != cfg
		runtime.Unlock()
		if changed {
			return nil, errors.New("stop active plugin before changing plugin configuration")
		}
		runtime.Lock()
		runtime.config = cfg
		if runtime.ctx == nil || runtime.closed {
			runtime.ctx, runtime.cancel = context.WithCancel(context.Background())
		}
		runtime.closed = false
		runtime.Unlock()
		return object{"schema_version": 6, "metadata": object{"Name": provider, "Version": "0.3.20", "Author": "rex", "GitHubRepository": "https://github.com/rex-zsd/doubao-cpa-plugin", "ConfigFields": []any{object{"Name": "state_dir", "Type": "string", "Description": "Private account and task state directory"}, object{"Name": "helper_path", "Type": "string", "Description": "Absolute path to doubao-helper"}, object{"Name": "native_tools", "Type": "boolean", "Description": "Forward compatible native text Read, Edit, Write and foreground Bash to the caller; default false, retain MCP for incompatible tools"}}}, "capabilities": object{"auth_provider": true, "model_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions", "openai-response"}, "executor_output_formats": []string{"chat-completions"}}}, nil
	case "plugin.shutdown", "plugin.quiesce":
		closePlugin()
		return object{}, nil
	case "auth.identifier", "executor.identifier":
		return object{"identifier": provider}, nil
	case "auth.parse":
		var data object
		if json.Unmarshal(req.RawJSON, &data) != nil || data["type"] != provider {
			return object{"Handled": false}, nil
		}
		return object{"Handled": true, "Auth": authRecord(req.FileName)}, nil
	case "auth.login.start":
		return startLogin()
	case "auth.login.poll":
		return pollLogin(req.State)
	case "auth.refresh":
		s, err := openService()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err = s.Validate(ctx); err != nil {
			return nil, &statusError{401, "Doubao login expired; sign in again"}
		}
		return object{"Auth": authRecord(req.AuthID), "NextRefreshAfter": time.Now().Add(30 * time.Minute)}, nil
	case "model.static":
		return object{"Provider": provider, "Models": []any{}}, nil
	case "model.for_auth":
		if req.AuthProvider != "" && req.AuthProvider != provider {
			return object{"Provider": provider, "Models": []any{}}, nil
		}
		s, err := openService()
		if err != nil {
			return nil, err
		}
		models := []any{}
		for _, m := range s.Models {
			models = append(models, modelInfo(m))
		}
		return object{"Provider": provider, "Models": models}, nil
	case "executor.execute", "executor.execute_stream":
		if req.AuthProvider != provider {
			return nil, errors.New("credential belongs to a different provider")
		}
		s, err := openService()
		if err != nil {
			return nil, err
		}
		payload, err := executorPayload(req)
		if err != nil {
			return nil, err
		}
		response, err := s.Submit(context.Background(), payload, req.Headers.Get("Idempotency-Key"))
		if err != nil {
			return nil, err
		}
		runtime.Lock()
		if runtime.closed {
			runtime.Unlock()
			return nil, &statusError{503, "Plugin is shutting down"}
		}
		streamCtx := runtime.ctx
		runtime.streams.Add(1)
		runtime.Unlock()
		if method == "executor.execute" {
			defer runtime.streams.Done()
			body, err := response.Complete(streamCtx)
			if err != nil {
				return nil, err
			}
			return object{"Payload": body, "Headers": http.Header{"Content-Type": {"application/json"}, "X-Doubao-Usage": {"estimated"}}}, nil
		}
		if req.StreamID == "" {
			runtime.streams.Done()
			return nil, errors.New("missing host stream identity")
		}
		translated := translatedStream(req)
		go func() {
			defer runtime.streams.Done()
			err := response.Stream(streamCtx, func(data []byte) error {
				frame, err := executorFrame(data, translated)
				if err != nil {
					return err
				}
				if len(frame) == 0 {
					return nil
				}
				return hostCall("host.stream.emit", object{"stream_id": req.StreamID, "payload": frame}, nil)
			})
			close := object{"stream_id": req.StreamID}
			if err != nil {
				close["error"] = streamFailure(err)
			}
			_ = hostCall("host.stream.close", close, nil)
		}()
		return object{"headers": http.Header{"Content-Type": {"text/event-stream"}, "X-Doubao-Usage": {"estimated"}}}, nil
	case "executor.count_tokens":
		return nil, &statusError{400, "Exact token counting is unavailable for Doubao Work"}
	case "executor.http_request":
		return nil, &statusError{400, "Use the supported model execution interfaces"}
	default:
		return nil, fmt.Errorf("unsupported plugin method: %s", method)
	}
}

type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string   { return e.message }
func (e *statusError) StatusCode() int { return e.status }

// CPA forwards chat-completions chunks verbatim inside its own SSE envelope.
// Its converters for every other client format read SSE "data:" lines instead,
// and the Claude one drops anything else. CPA always adds the terminal event.
func executorFrame(data []byte, translated bool) ([]byte, error) {
	frame := bytes.TrimSpace(data)
	if bytes.HasPrefix(frame, []byte(":")) {
		return nil, nil
	}
	if bytes.HasPrefix(frame, []byte("data:")) {
		frame = bytes.TrimSpace(frame[5:])
	}
	if bytes.Equal(frame, []byte("[DONE]")) {
		return nil, nil
	}
	if !json.Valid(frame) {
		return nil, errors.New("invalid executor JSON frame")
	}
	if translated {
		return append([]byte("data: "), frame...), nil
	}
	return frame, nil
}

// translatedStream reports whether CPA converts this stream for a client that
// does not speak chat completions, judged by the route the request came in on.
func translatedStream(req request) bool {
	path, _ := req.Metadata["request_path"].(string)
	if path == "" {
		return req.Headers.Get("Anthropic-Version") != ""
	}
	return !strings.HasSuffix(path, "/completions")
}
