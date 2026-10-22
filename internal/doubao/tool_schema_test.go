package doubao

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestToolSchemaValidatesReferencesAndExactLargeNumbers(t *testing.T) {
	var parameters object
	if err := decode(strings.NewReader(`{"type":"object","$defs":{"count":{"type":"integer","minimum":9007199254740993}},"properties":{"n":{"$ref":"#/$defs/count"},"mode":{"enum":["a","b"]}},"required":["n","mode"],"additionalProperties":false}`), &parameters); err != nil {
		t.Fatal(err)
	}
	schema, err := compiledToolSchema(apiTool{Function: apiFunction{Parameters: parameters}})
	if err != nil {
		t.Fatal(err)
	}
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	req.Tools[0].Function.Parameters = parameters
	if !compactBatchMode(req) || len(toolCatalog(req)) != 2 {
		t.Fatal("nesting a reference schema changed its original reference scope")
	}
	if err = schema.Validate(object{"n": json.Number("9007199254740993"), "mode": "a"}); err != nil {
		t.Fatal("valid precision-preserving arguments refused", err)
	}
	for _, arguments := range []object{
		{"n": json.Number("9007199254740992"), "mode": "a"},
		{"n": "9007199254740993", "mode": "a"},
		{"n": json.Number("9007199254740993"), "mode": "unknown"},
		{"n": json.Number("9007199254740993"), "mode": "a", "extra": true},
		{"n": json.Number("9007199254740993")},
	} {
		if schema.Validate(arguments) == nil {
			t.Fatal("invalid arguments accepted", arguments)
		}
	}
}

func TestUnsupportedSchemaKeepsItsOriginalFunctionAndOtherBatches(t *testing.T) {
	req := toolRequest()
	parallel := true
	req.Parallel = &parallel
	for i := 0; i < 11; i++ {
		req.Tools = append(req.Tools, apiTool{Type: "function", Function: apiFunction{Name: fmt.Sprintf("lookup_%d", i), Description: strings.Repeat("x", 5000), Parameters: object{"type": "object"}}})
	}
	req.Tools[0].Function.Parameters = object{"type": "object", "properties": object{"x": object{"$ref": "file:///private/cannot-read.json"}}}
	if !batchMode(req) || !compactBatchMode(req) || len(toolCatalog(req)) != len(req.Tools)+len(batchRoutes(req)) {
		t.Fatal("one unsupported schema disabled other batches or lost its original route")
	}
	for _, tool := range batchTools(req) {
		if tool.Function.Name == "client_nonce" {
			t.Fatal("unverifiable schema entered generic batching")
		}
	}
	req.Tools = req.Tools[:1]
	if batchMode(req) || len(toolCatalog(req)) != 1 {
		t.Fatal("an entirely unsupported catalog lost original single calls")
	}
}

func TestSchemaCacheCompilesCanonicalJSONRegardlessOfGoArrayType(t *testing.T) {
	tool := apiTool{Function: apiFunction{Parameters: object{"type": "object", "properties": object{"canonical_cache_test": object{"type": "string"}}, "required": []string{"canonical_cache_test"}}}}
	schema, err := compiledToolSchema(tool)
	if err != nil || schema.Validate(object{"canonical_cache_test": "ok"}) != nil {
		t.Fatal("Go array representation poisoned canonical schema cache", err)
	}
	var normalized object
	if err = decode(strings.NewReader(jsonText(tool.Function.Parameters)), &normalized); err != nil {
		t.Fatal(err)
	}
	tool.Function.Parameters = normalized
	again, err := compiledToolSchema(tool)
	if err != nil || again != schema {
		t.Fatal("equal JSON schemas received different cache results", err)
	}
}
