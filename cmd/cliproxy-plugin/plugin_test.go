package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"doubao-cpa-plugin/internal/doubao"
	"gopkg.in/yaml.v3"
)

func TestExecutorPayloadUsesResolvedModelAndPreservesRequest(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"keep the full history"}],"reasoning_effort":7,"parallel_tool_calls":true,"tools":[{"type":"function","function":{"name":"Bash","description":"Keep this exact description.","parameters":{"type":"object","properties":{"count":{"type":"integer","default":9007199254740993}}}}}]}`)
	out, err := executorPayload(request{Model: "doubao-orange-5.5", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal(payload, &before) != nil || json.Unmarshal(out, &after) != nil {
		t.Fatal("invalid normalized payload")
	}
	if string(after["model"]) != `"doubao-orange-5.5"` {
		t.Fatal("client alias reached the upstream model selector", string(after["model"]))
	}
	delete(before, "model")
	delete(after, "model")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("alias resolution changed history, reasoning or tool definitions")
	}
	for _, req := range []request{{Payload: payload}, {Model: "claude-opus-5-5", Payload: payload}, {Model: "doubao-orange-5.5", Payload: []byte("not json")}, {Model: "doubao-orange-5.5", Payload: []byte("null")}} {
		out, err := executorPayload(req)
		if err != nil || !bytes.Equal(out, req.Payload) {
			t.Fatal("unmapped or malformed payload changed", err)
		}
	}
}

func TestExecutorPayloadResolvesResponsesAliasAfterNormalization(t *testing.T) {
	out, err := executorPayload(request{SourceFormat: "openai-response", Model: "doubao-orange-5.5", Payload: []byte(`{"model":"claude-opus-5-5","input":"keep this input","reasoning":{"effort":"max"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Model    string
		Messages []struct{ Role, Content string }
		Effort   any `json:"reasoning_effort"`
	}
	if json.Unmarshal(out, &result) != nil || result.Model != "doubao-orange-5.5" || len(result.Messages) != 1 || result.Messages[0].Content != "keep this input" || result.Effort != "max" {
		t.Fatal("Responses alias normalization lost model, input or effort", string(out))
	}
}

func TestOfficialHostUsesNativeTransportByDefault(t *testing.T) {
	var cfg config
	if err := yaml.Unmarshal([]byte("state_dir: /state\nhelper_path: /bin/doubao-helper\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if options := serviceConfig(cfg); options.Transport != nil || options.StateDir != "/state" || options.HelperPath != "/bin/doubao-helper" {
		t.Fatalf("default config must not depend on the host HTTP bridge: %+v", options)
	}
}

func TestNativeToolsConfigIsExplicitAndKeepsConnectorReuse(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var cfg config
		data := "state_dir: /state\nhelper_path: /bin/doubao-helper\n"
		if enabled {
			data += "native_tools: true\n"
		}
		if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
			t.Fatal(err)
		}
		options := serviceConfig(cfg)
		if options.NativeTools != enabled || !options.ConnectorReuse || options.Transport != nil {
			t.Fatal("native routing changed transport, cache or opt-in semantics")
		}
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
