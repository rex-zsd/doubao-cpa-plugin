package doubao

import "fmt"

type batchRoute struct {
	name    string
	tools   []apiTool
	compact bool
}

// Keep each discovered batch small enough to include its original schemas in
// one search result. A generic envelope required another schema discovery and
// was slower on the live four-read comparison. References retain their scope
// through a separate compact route plus the original individual definitions.
const batchGroupTarget = 16000

func batchRoutes(req apiRequest) []batchRoute {
	if !parallelRequested(req) {
		return nil
	}
	used := map[string]bool{}
	for _, tool := range req.Tools {
		used[tool.Function.Name] = true
		if tool.Function.Name == batchToolName {
			return nil
		}
	}
	valid := batchTools(req)
	if len(valid) == 0 {
		return nil
	}
	scoped := false
	for _, tool := range valid {
		scoped = scoped || scopedSchema(tool.Function.Parameters)
	}
	if !scoped && len(valid) == len(req.Tools) && utf16Len(batchSchema(valid)) <= batchSchemaLimit {
		return []batchRoute{{name: batchToolName, tools: valid}}
	}
	routes := []batchRoute{}
	group, compact := []apiTool{}, []apiTool{}
	flush := func() {
		if len(group) > 0 {
			routes = append(routes, batchRoute{tools: group})
			group = nil
		}
	}
	for _, tool := range valid {
		if scopedSchema(tool.Function.Parameters) || utf16Len(batchSchema([]apiTool{tool})) > batchSchemaLimit {
			compact = append(compact, tool)
			continue
		}
		candidate := append(append([]apiTool{}, group...), tool)
		if len(group) > 0 && utf16Len(batchSchema(candidate)) > batchGroupTarget {
			flush()
		}
		group = append(group, tool)
	}
	flush()
	if len(compact) > 0 {
		routes = append(routes, batchRoute{tools: compact, compact: true})
	}
	for i := range routes {
		name := batchToolName
		if len(routes) > 1 {
			name = fmt.Sprintf("%s_%d", batchToolName, i+1)
		}
		for n := 1; used[name]; n++ {
			name = fmt.Sprintf("%s_%d_%d", batchToolName, i+1, n)
		}
		used[name] = true
		routes[i].name = name
	}
	return routes
}

func retainOriginalTools(req apiRequest, routes []batchRoute) bool {
	return len(routes) > 0 && !(len(routes) == 1 && !routes[0].compact && routes[0].name == batchToolName && len(routes[0].tools) == len(req.Tools))
}

func batchRouteFor(req apiRequest, name string) (batchRoute, bool) {
	for _, route := range batchRoutes(req) {
		if route.name == name {
			return route, true
		}
	}
	return batchRoute{}, false
}

func toolCatalog(req apiRequest) []any {
	routes := batchRoutes(req)
	tools := []any{}
	if len(routes) == 0 || retainOriginalTools(req, routes) {
		for _, tool := range req.Tools {
			tools = append(tools, object{"name": tool.Function.Name, "description": connectorDescription(tool), "inputSchema": jsonText(connectorSchema(tool))})
		}
	}
	for _, route := range routes {
		schema := batchSchema(route.tools)
		description := "Execute 1 to 8 independent client functions in one parallel batch. This input schema contains the original descriptions and argument constraints for every function in this group; use those definitions directly. Separate original-function invocations are serialized by this adapter. Wait for real results. Dependent calls belong in a later batch. Results identify each call by index and function name."
		if route.compact {
			names := []string{}
			for _, tool := range route.tools {
				names = append(names, tool.Function.Name)
			}
			schema = jsonText(object{"type": "object", "properties": object{"calls": object{"type": "array", "minItems": 1, "maxItems": 8, "items": object{"type": "object", "properties": object{"name": object{"type": "string", "enum": names}, "arguments": object{"type": "object"}}, "required": []string{"name", "arguments"}, "additionalProperties": false}}}, "required": []string{"calls"}, "additionalProperties": false})
			description = "Execute 1 to 8 independent client functions in parallel. This group has schemas whose reference scope or size prevents inlining. Discover each original function's full JSON Schema first; arguments are validated against that original. Dependent calls belong in a later batch. Results identify each input by index and function name."
		}
		tools = append(tools, object{"name": route.name, "description": description, "inputSchema": schema})
	}
	return tools
}
