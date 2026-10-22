package doubao

import (
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Schemas are immutable and addressed by their entire canonical JSON value.
// Bounds apply to retained source size and entry count. External references
// cannot read local files or fetch URLs; unsupported catalogs keep single calls.
var toolSchemas = struct {
	sync.Mutex
	items map[string]compiledSchema
	bytes int
}{items: map[string]compiledSchema{}}

type compiledSchema struct {
	schema *jsonschema.Schema
	err    error
}

func compiledToolSchema(tool apiTool) (*jsonschema.Schema, error) {
	key := apiHash(tool.Function.Parameters)
	toolSchemas.Lock()
	defer toolSchemas.Unlock()
	if entry, ok := toolSchemas.items[key]; ok {
		return entry.schema, entry.err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(jsonschema.SchemeURLLoader{})
	location := "https://doubao.invalid/schema/" + key
	// Compile the same canonical JSON the cache key describes. Go-created
	// schemas can contain []string (rather than JSON's []any); accepting one
	// representation but caching an error for the other makes order matter.
	source := jsonText(tool.Function.Parameters)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(source))
	if err == nil {
		err = c.AddResource(location, doc)
	}
	var schema *jsonschema.Schema
	if err == nil {
		schema, err = c.Compile(location)
	}
	size := len(source)
	if len(toolSchemas.items) >= 64 || toolSchemas.bytes+size > 4<<20 {
		toolSchemas.items, toolSchemas.bytes = map[string]compiledSchema{}, 0
	}
	toolSchemas.items[key] = compiledSchema{schema, err}
	toolSchemas.bytes += size
	return schema, err
}
