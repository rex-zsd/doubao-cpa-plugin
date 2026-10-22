package doubao

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestGroupedBatchSchemasKeepEveryDefinitionAndRejectOtherGroups(t *testing.T) {
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	for i := 0; i < 12; i++ {
		req.Tools = append(req.Tools, apiTool{Type: "function", Function: apiFunction{Name: fmt.Sprintf("lookup_%d", i), Description: strings.Repeat("x", 5000), Parameters: object{"type": "object", "properties": object{"q": object{"type": "integer"}}, "required": []string{"q"}}}})
	}
	routes := batchRoutes(req)
	if len(routes) < 2 {
		t.Fatal("large catalog was not grouped")
	}
	seen := map[string]bool{}
	catalog := toolCatalog(req)
	for i, route := range routes {
		if route.compact || utf16Len(textValue(field(catalog[len(req.Tools)+i], "inputSchema"))) > batchSchemaLimit {
			t.Fatal("group requires a second discovery or exceeds limit")
		}
		for _, tool := range route.tools {
			if seen[tool.Function.Name] {
				t.Fatal("function occurs in multiple groups")
			}
			seen[tool.Function.Name] = true
			if !strings.Contains(textValue(field(catalog[len(req.Tools)+i], "inputSchema")), jsonText(tool.Function.Parameters)) {
				t.Fatal("group lost original parameter definition")
			}
		}
	}
	if len(seen) != len(req.Tools) {
		t.Fatal("valid function missing from batch groups")
	}
	// A valid function from another group must not bypass this route's schema.
	g, _ := testGateway(t)
	turn := &gatewayTurn{id: "0123456789abcdef0123456789abcdef", request: req, connector: "c"}
	other := routes[len(routes)-1].tools[0].Function.Name
	result, err := g.forwardClientTool(context.Background(), turn, object{"id": "n", "name": "connector.call"}, object{"protocol_version": 1, "connector_id": "c", "remote_tool_name": routes[0].name, "raw_arguments": jsonText(object{"calls": []any{object{"name": other, "arguments": object{"q": 1}}}})})
	if err != nil || result["isError"] != true || len(g.calls) != 0 {
		t.Fatal("cross-group arguments were accepted", err)
	}
}
