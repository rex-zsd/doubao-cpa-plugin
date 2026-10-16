package doubao

import (
	"strconv"
	"strings"
)

// Responses clients such as Codex declare tools inside namespaces, and Codex
// on Responses Lite sends them in an additional_tools input item. CPA turns
// the chat calls this plugin returns back into Responses items by looking
// their names up in the original request, so every tool must get exactly the
// chat name CPA's own Responses translator would give it (CPA v8.0.13,
// openai_openai-responses_tools.go): namespace and name joined by "__",
// capped to 64 characters from the tail, with "_N" suffixes for names that
// only collide through the cap.
const responsesToolNameLimit = 64

type responsesTool struct {
	declaration                      any
	chat, local, namespace, identity string
	custom                           bool
}

type responsesToolIndex struct {
	tools      []responsesTool // the first declaration of each chat name
	byChat     map[string]bool
	byIdentity map[[2]string]string
	byRaw      map[string]string
	byLocal    map[string]string // "" when several chat names share it
}

func qualifiedToolName(namespace, name string) string {
	switch name = strings.TrimSpace(name); {
	case name == "" || namespace == "" || strings.HasPrefix(name, "mcp__") || name == namespace || strings.HasPrefix(name, namespace+"__"):
		return name
	case strings.HasSuffix(namespace, "__"):
		return namespace + name
	}
	return namespace + "__" + name
}

// cappedToolName keeps the tail, which carries the tool's own name.
func cappedToolName(name string) string {
	if len(name) <= responsesToolNameLimit {
		return name
	}
	name = name[len(name)-responsesToolNameLimit:]
	if trimmed := strings.TrimLeft(name, "_-"); trimmed != "" {
		return trimmed
	}
	return name
}

// newResponsesToolIndex reads the top-level tools first, then every
// additional_tools item, so the top-level copy of a tool sent both ways wins.
func newResponsesToolIndex(tools []any, items []any) *responsesToolIndex {
	declared := []responsesTool{}
	emit := func(tool any, namespace string) {
		kind := strings.TrimSpace(textValue(field(tool, "type")))
		if kind != "" && kind != "function" && kind != "custom" {
			// Hosted tools such as web or file search run on OpenAI's servers and
			// Work has no equivalent, so only caller tools are offered.
			return
		}
		name := strings.TrimSpace(textValue(field(tool, "name")))
		if name == "" {
			name = strings.TrimSpace(textValue(field(tool, "function", "name")))
		}
		if name != "" {
			identity := qualifiedToolName(namespace, name)
			declared = append(declared, responsesTool{tool, cappedToolName(identity), name, namespace, identity, kind == "custom"})
		}
	}
	scan := func(tools []any) {
		for _, tool := range tools {
			if strings.TrimSpace(textValue(field(tool, "type"))) != "namespace" {
				emit(tool, "")
				continue
			}
			namespace := strings.TrimSpace(textValue(field(tool, "name")))
			for _, child := range array(field(tool, "tools")) {
				emit(child, namespace)
			}
		}
	}
	scan(tools)
	for _, item := range items {
		if field(item, "type") == "additional_tools" {
			scan(array(field(item, "tools")))
		}
	}
	disambiguateToolNames(declared)
	idx := &responsesToolIndex{byChat: map[string]bool{}, byIdentity: map[[2]string]string{}, byRaw: map[string]string{}, byLocal: map[string]string{}}
	for _, t := range declared {
		if _, ok := idx.byIdentity[[2]string{t.namespace, t.local}]; !ok {
			idx.byIdentity[[2]string{t.namespace, t.local}] = t.chat
		}
		if _, ok := idx.byRaw[t.identity]; !ok {
			idx.byRaw[t.identity] = t.chat
		}
		if idx.byChat[t.chat] {
			continue
		}
		idx.byChat[t.chat] = true
		idx.tools = append(idx.tools, t)
		if _, ok := idx.byLocal[t.local]; ok {
			idx.byLocal[t.local] = ""
		} else {
			idx.byLocal[t.local] = t.chat
		}
	}
	return idx
}

// disambiguateToolNames renames only declarations whose capped names collide.
// Names that fit unchanged, and local names that a call without its namespace
// may still use, are claimed first; a local name declared under two
// namespaces is never handed out.
func disambiguateToolNames(tools []responsesTool) {
	claimed := map[string]string{}
	claim := func(name, identity string) bool {
		owner, taken := claimed[name]
		if !taken {
			claimed[name] = identity
		}
		return !taken || owner == identity
	}
	long := []int{}
	owners := map[string]string{}
	for i, t := range tools {
		if len(t.identity) > responsesToolNameLimit {
			long = append(long, i)
		} else {
			claim(t.identity, t.identity)
		}
		if t.local == t.identity || len(t.local) > responsesToolNameLimit {
			continue
		}
		if owner, seen := owners[t.local]; !seen {
			owners[t.local] = t.identity
		} else if owner != t.identity {
			owners[t.local] = ""
		}
	}
	ambiguous := map[string]bool{}
	for local, owner := range owners {
		claim(local, owner)
		ambiguous[local] = owner == ""
	}
	for _, i := range long {
		name := tools[i].chat
		if !ambiguous[name] && claim(name, tools[i].identity) {
			continue
		}
		for n := 1; ; n++ {
			if candidate := cappedToolName(name + "_" + strconv.Itoa(n)); !ambiguous[candidate] && claim(candidate, tools[i].identity) {
				tools[i].chat = candidate
				break
			}
		}
	}
}

// callName resolves a replayed call or forced tool_choice. Without a
// namespace, an emitted name wins, then a qualified one, then a local name
// only one tool declares.
func (idx *responsesToolIndex) callName(namespace, name string) string {
	if namespace = strings.TrimSpace(namespace); namespace != "" {
		if chat, ok := idx.byIdentity[[2]string{namespace, name}]; ok {
			return chat
		}
		return idx.unclaimed(cappedToolName(qualifiedToolName(namespace, name)))
	}
	if idx.byChat[name] {
		return name
	}
	if chat, ok := idx.byRaw[name]; ok {
		return chat
	}
	if chat := idx.byLocal[name]; chat != "" {
		return chat
	}
	return idx.unclaimed(cappedToolName(name))
}

// unclaimed keeps an unresolved name from dispatching to a declared tool.
func (idx *responsesToolIndex) unclaimed(name string) string {
	for n, candidate := 1, name; ; n++ {
		if !idx.byChat[candidate] {
			return candidate
		}
		candidate = cappedToolName(name + "_" + strconv.Itoa(n))
	}
}

// function returns the chat function CPA's translator derives from t.
func (t responsesTool) function() object {
	description := textValue(field(t.declaration, "description"))
	if description == "" {
		description = textValue(field(t.declaration, "function", "description"))
	}
	if t.custom {
		// Freeform tools such as Codex apply_patch take raw text, so they are
		// offered as a function of one string "input": the shape their
		// history is replayed in and that CPA unwraps on the way back.
		description = strings.TrimSpace(strings.ReplaceAll(description, "This is a FREEFORM tool, so do not wrap the patch in JSON.", ""))
		if definition := textValue(field(t.declaration, "format", "definition")); definition != "" {
			description += "\n\nPut the raw text in the input field. It must follow this " + textValue(field(t.declaration, "format", "syntax")) + " grammar:\n" + definition
		}
		return object{"name": t.chat, "description": description, "parameters": object{"type": "object", "properties": object{"input": object{"type": "string"}}, "required": []any{"input"}, "additionalProperties": false}}
	}
	fn := object{"name": t.chat, "description": description}
	for _, path := range [][]string{{"parameters"}, {"parametersJsonSchema"}, {"input_schema"}, {"function", "parameters"}, {"function", "parametersJsonSchema"}} {
		if parameters := field(t.declaration, path...); parameters != nil {
			fn["parameters"] = parameters
			break
		}
	}
	if strict := field(t.declaration, "strict"); strict != nil {
		fn["strict"] = strict
	}
	return fn
}
