package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"doubao-cpa-plugin/internal/doubao"
	"gopkg.in/yaml.v3"
)

func TestOfficialHostUsesNativeTransportByDefault(t *testing.T) {
	var cfg config
	if err := yaml.Unmarshal([]byte("state_dir: /state\nhelper_path: /bin/doubao-helper\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if options := serviceConfig(cfg); options.Transport != nil || options.StateDir != "/state" || options.HelperPath != "/bin/doubao-helper" {
		t.Fatalf("default config must not depend on the host HTTP bridge: %+v", options)
	}
}

func TestExecutorSSEFramingIsOwnedByCPA(t *testing.T) {
	for translated, want := range map[bool]string{false: `{"object":"chat.completion.chunk"}`, true: `data: {"object":"chat.completion.chunk"}`} {
		frame, err := executorFrame([]byte("data: {\"object\":\"chat.completion.chunk\"}\n\n"), translated)
		if err != nil || string(frame) != want {
			t.Fatal(translated, string(frame), err)
		}
		for _, input := range []string{": keepalive\n\n", "data: [DONE]\n\n"} {
			frame, err := executorFrame([]byte(input), translated)
			if err != nil || len(frame) != 0 {
				t.Fatal("control frame forwarded as JSON", input)
			}
		}
	}
}

func TestOnlyChatCompletionClientsGetBareChunks(t *testing.T) {
	for _, c := range []struct {
		path, version string
		translated    bool
	}{
		{"/v1/chat/completions", "", false},
		{"/v1/completions", "", false},
		{"/api/provider/:provider/v1/chat/completions", "", false},
		{"/v1/messages", "2023-06-01", true},
		{"/v1/responses", "", true},
		{"/backend-api/codex/responses", "", true},
		{"/v1beta/models/*action", "", true},
		{"", "2023-06-01", true},
		{"", "", false},
	} {
		req := request{Headers: http.Header{}}
		if c.path != "" {
			req.Metadata = map[string]any{"request_path": c.path}
		}
		if c.version != "" {
			req.Headers.Set("Anthropic-Version", c.version)
		}
		if got := translatedStream(req); got != c.translated {
			t.Fatalf("%q: translated=%v", c.path, got)
		}
	}
}

type capacityError struct{}

func (capacityError) Error() string     { return "Gateway capacity reached" }
func (capacityError) StatusCode() int   { return 429 }
func (capacityError) ErrorCode() string { return "gateway_capacity" }
func (capacityError) ErrorType() string { return "invalid_request_error" }

func TestTypedErrorsReachClientsAsJSON(t *testing.T) {
	_, invalid := doubao.NormalizeResponses([]byte("not json"))
	for _, c := range []struct {
		err          error
		status       int
		code, typeOf string
	}{{invalid, 400, "invalid_json", "invalid_request_error"}, {capacityError{}, 429, "gateway_capacity", "rate_limit_error"}} {
		var envelope struct {
			Error struct {
				Code, Message string
				Status        int `json:"http_status"`
			}
		}
		var body struct {
			Error struct{ Message, Type, Code string }
		}
		if json.Unmarshal(failureFrom(c.err), &envelope) != nil || json.Unmarshal([]byte(envelope.Error.Message), &body) != nil {
			t.Fatal("failure is not a JSON error body", c.err)
		}
		if envelope.Error.Status != c.status || body.Error.Code != c.code || body.Error.Type != c.typeOf || body.Error.Message != c.err.Error() {
			t.Fatal("typed error lost its status, code or type", envelope, body)
		}
		if streamFailure(c.err) != envelope.Error.Message {
			t.Fatal("stream close error differs from the request error", streamFailure(c.err))
		}
	}
	if string(failureFrom(errors.New("boom"))) != string(failure("doubao_error", "boom", 500)) || streamFailure(errors.New("boom")) != "Doubao stream interrupted" {
		t.Fatal("untyped errors must stay opaque")
	}
}

func TestModelInfoAdvertisesWorkLimits(t *testing.T) {
	info := modelInfo(doubao.PublicModel{ID: "doubao-gpt-6.1-sol", DisplayName: "GPT-6.1 Sol", Window: 400000, Efforts: []int{3, 4, 5, 6, 7}})
	if info["ContextLength"] != 65536 || !reflect.DeepEqual(info["Thinking"], object{"Levels": []string{"low", "medium", "high", "xhigh", "max"}}) || !strings.Contains(info["Description"].(string), "262144") {
		t.Fatal("model limits not advertised", info)
	}
	info = modelInfo(doubao.PublicModel{ID: "doubao-auto", DisplayName: "自动", Window: 32000})
	if info["ContextLength"] != 32000 || info["Thinking"] != nil {
		t.Fatal("a smaller window or missing levels was ignored", info)
	}
	if field := authRecord("doubao-work.json")["Metadata"].(object)["disable_cooling"]; field != true {
		t.Fatal("one account per state_dir must not be cooled down")
	}
}
