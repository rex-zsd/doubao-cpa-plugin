package doubao

import (
	"context"
	"strings"
	"testing"
)

// Misrouted calls must remain rejected: a recovery hint is not permission to
// execute a native call as a client function or to use another task's connector.
func TestRejectedNativeToolsSuggestOnlyCurrentDeclaredRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, native, wanted, absent string
		parallel                     bool
		input                        object
	}{
		{name: "native shell", native: "Bash", wanted: "__bash", absent: "__read"},
		{name: "computer fallback", native: "computer_use_tool", wanted: "__read"},
		{name: "foreign connector", native: "connector.call", wanted: "__bash", input: object{"protocol_version": 1, "connector_id": "foreign", "remote_tool_name": "Bash", "raw_arguments": `{}`}},
		{name: "batch only", native: "Bash", parallel: true, wanted: "__client_tools_batch", absent: "__bash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := testGateway(t)
			req := toolRequest()
			req.Tools = []apiTool{
				{Type: "function", Function: apiFunction{Name: "Bash", Parameters: object{"type": "object"}}},
				{Type: "function", Function: apiFunction{Name: "Read", Parameters: object{"type": "object"}}},
			}
			req.Parallel = &tc.parallel
			turn := &gatewayTurn{id: "0123456789abcdef0123456789abcdef", connector: "current-connector", request: req}
			call := object{"id": "native-call", "name": tc.native, "input": "original input"}
			result, err := g.forwardClientTool(context.Background(), turn, call, tc.input)
			if err != nil || result["isError"] != true || result["error_code"] != "CLIENT_TOOL_ONLY" {
				t.Fatal("native call was not rejected", err, result)
			}
			if turn.forwarded != 0 || len(g.calls) != 0 || turn.denied != 1 || result["id"] != call["id"] || result["tool_input"] != call["input"] {
				t.Fatal("rejection changed the execution boundary", result)
			}
			hint := textValue(result["error_msg"])
			if !strings.Contains(hint, "tool_search") || !strings.Contains(hint, "mcp__independent_http_tools_0123456789ab"+tc.wanted) || strings.Contains(hint, "foreign") || tc.absent != "" && strings.Contains(hint, tc.absent) {
				t.Fatal("hint offers an incorrect or unavailable route", hint)
			}
		})
	}
}
