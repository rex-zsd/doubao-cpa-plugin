package doubao

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNonceMCPNotificationsAndRealCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_nonce","arguments":{"challenge":"fixture"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"Bash","arguments":{"challenge":"fixture"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := serveNonce(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(rows) != 4 {
		t.Fatal("notification received a response", output.String())
	}
	var listing, call, denied object
	decode(strings.NewReader(rows[1]), &listing)
	decode(strings.NewReader(rows[2]), &call)
	decode(strings.NewReader(rows[3]), &denied)
	if strings.Contains(rows[1], "DOUBAO_TOOL_") {
		t.Fatal("nonce leaked before tools/call")
	}
	if len(array(field(listing, "result", "tools"))) != 1 || field(call, "result", "isError") != false || denied["error"] == nil {
		t.Fatal("invalid MCP behavior")
	}
	value := parsed(field(array(field(call, "result", "content"))[0], "text"))
	if field(value, "challenge") != "fixture" || !strings.HasPrefix(textValue(field(value, "nonce")), "DOUBAO_TOOL_") {
		t.Fatal("tool did not generate a nonce")
	}
}

func TestWorkspaceInitializationConfinesPathsAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	request := func(path string) object {
		return object{"request_id": "r", "workspace": object{"root": root, "agent_id": "a", "session_id": "s"}, "operations": []any{object{"type": "write_file", "mode": "replace", "path": path, "content": "fixture"}}}
	}
	if _, ok := applyProbeWorkspace(root, request("nested/ok")); !ok {
		t.Fatal("valid initialization failed")
	}
	for _, path := range []string{"../outside/bad", filepath.Join(outside, "bad"), "escape/bad"} {
		if _, ok := applyProbeWorkspace(root, request(path)); ok {
			t.Fatal("accepted an escaping path", path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "bad")); !os.IsNotExist(err) {
		t.Fatal("escaped workspace")
	}
}

func TestNativeToolScopeDeduplicationAndSerializedResult(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	invoked, uploaded := 0, 0
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		uploaded++
		var body object
		if err := decode(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Content-Type") != "application/json" || field(body, "tool_result", "raw_result") == nil {
			t.Fatal("invalid native upload DTO")
		}
		if _, ok := field(body, "tool_result", "raw_result").(string); !ok {
			t.Fatal("raw_result must be serialized JSON")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	p := &toolProbe{c: c, sandbox: "owned", connector: "connector", challenge: "challenge", journal: filepath.Join(c.dir, "journal.json"), events: map[string]int{}, records: map[string]object{}}
	p.invoke = func(context.Context, string, object) (object, error) {
		invoked++
		return object{"content": []any{object{"type": "text", "text": `{"nonce":"DOUBAO_TOOL_fixture","challenge":"challenge"}`}}, "isError": false}, nil
	}
	call := object{"id": "call-1", "name": "connector.call", "input": jsonText(object{"protocol_version": 1, "connector_id": "connector", "remote_tool_name": "get_nonce", "raw_arguments": `{"challenge":"challenge"}`})}
	frame := object{"event_type": 1, "sandbox_id": "foreign", "tool_call": call}
	if err = p.handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if invoked != 0 || uploaded != 0 {
		t.Fatal("executed another sandbox's tool")
	}
	frame["sandbox_id"] = "owned"
	if err = p.handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if err = p.handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if invoked != 1 || uploaded != 1 || len(p.proofs) != 1 || p.proofs[0]["result_uploaded"] != true {
		t.Fatal("duplicate execution or missing upload", invoked, uploaded)
	}
	call["input"] = `{}`
	if err = p.handle(context.Background(), frame); err == nil {
		t.Fatal("call ID collision was accepted")
	}
}

func TestLocalToolRequestKeepsSchemaOutsideUserPrompt(t *testing.T) {
	body := localToolBody("fixture", Model{ID: "1", Think: 1}, "/owned", "sandbox", "123", "456", []any{object{"name": "get_nonce", "inputSchema": jsonText(nonceTool()["inputSchema"])}})
	gt := field(body, "option", "general_task_param")
	if number(field(gt, "runtime_type")) != 2 || field(body, "ext", "general_task_param") != jsonText(gt) {
		t.Fatal("local runtime not configured consistently")
	}
	input := parsed(field(gt, "task_input_json"))
	connectors := array(field(input, "localConnectors"))
	if len(connectors) != 1 || field(connectors[0], "connectorId") != "456" || len(array(field(connectors[0], "mcpToolsList"))) != 1 {
		t.Fatal("tool schema was not exposed natively")
	}
}
