package doubao

import (
	"path"
	"strings"
	"time"
)

func nativeBuiltinsRule(probe bool) string {
	if probe {
		return "只有下文原生工具转发明确列出的同名工具可直接调用；未列出或参数不兼容的工具必须使用调用方 MCP 函数。其他平台内置工具不可用；与平台工具同名但未在下文列出的调用方函数也须走 MCP。"
	}
	return "不要改用你所在平台自带的 Bash、Read、Write、Edit、computer_use_tool 等内置工具代替这些函数；这些内置工具在此环境不可用，重试、更换权限或通过 Python subprocess 调用也不会成功。这是工具路由的区别，不影响调用方 MCP 函数的可用性。"
}

func nativeDiscoveryRule(probe bool) string {
	if probe {
		return "单次或依赖链中，下文原生转发范围内的同名工具优先直接调用；独立调用优先 MCP 并行批入口。其他函数使用前，先调用"
	}
	return "使用其中一个函数前，先调用"
}

// This is a deliberately narrow compatibility route. A matching name is
// insufficient: preserve the caller's schema, arguments, descriptions, execution
// environment and actual results. No native shell or reader runs in this helper.
func nativeCompatibleTool(tool apiTool) bool {
	name := tool.Function.Name
	schema := tool.Function.Parameters
	properties := field(schema, "properties")
	fields := nativeTextFields(name)
	if fields == nil || schema["type"] != "object" {
		return false
	}
	for _, key := range fields {
		if field(properties, key, "type") != "string" {
			return false
		}
	}
	for _, key := range array(parsed(jsonText(schema["required"]))) {
		if !nativeArgumentKey(name, textValue(key)) {
			return false
		}
	}
	compiled, err := compiledToolSchema(tool)
	return err == nil && compiled != nil
}

// nativeTextFields lists the arguments each forwarded platform tool depends on;
// the caller's schema must type them as strings. Other names never forward.
func nativeTextFields(name string) []string {
	switch name {
	case "Read":
		return []string{"file_path"}
	case "Write":
		return []string{"file_path", "content"}
	case "Edit":
		return []string{"file_path", "old_string", "new_string"}
	case "NotebookEdit":
		return []string{"notebook_path", "new_source"}
	case "Bash":
		return []string{"command"}
	case "Glob", "Grep":
		return []string{"pattern"}
	case "TaskOutput", "TaskStop":
		return []string{"task_id"}
	}
	return nil
}

func nativeToolName(name string) bool { return nativeTextFields(name) != nil }

// Only arguments with the same meaning on both sides. Work-only Glob options
// (ordering, limits, entry kinds) and aliases such as Grep include or
// TaskOutput shell_id are absent, so such calls keep the MCP route.
func nativeArgumentKey(name, key string) bool {
	switch name {
	case "Read":
		return key == "file_path" || key == "offset" || key == "limit"
	case "Write":
		return key == "file_path" || key == "content"
	case "Edit":
		return key == "file_path" || key == "old_string" || key == "new_string" || key == "replace_all"
	case "NotebookEdit":
		return key == "notebook_path" || key == "cell_id" || key == "cell_type" || key == "edit_mode" || key == "new_source"
	case "Bash":
		return key == "command" || key == "description" || key == "timeout" || key == "run_in_background" || key == "dangerouslyDisableSandbox"
	case "Glob":
		return key == "pattern" || key == "path"
	case "Grep":
		switch key {
		case "pattern", "path", "glob", "type", "output_mode", "-i", "-n", "-A", "-B", "-C", "context", "multiline", "head_limit", "offset":
			return true
		}
	case "TaskOutput":
		return key == "task_id" || key == "block" || key == "timeout"
	case "TaskStop":
		return key == "task_id" || key == "shell_id"
	}
	return false
}

// Read results are interpreted as text on this route. The helper cannot inspect
// a client file, so only known text names are offered; media/PDF/notebooks and
// unknown formats retain the original MCP contract. Paths are never rewritten.
func nativeTextPath(file string) bool {
	switch strings.ToLower(path.Ext(file)) {
	case ".txt", ".go", ".mod", ".sum", ".py", ".pyi", ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".md", ".mdx", ".json", ".jsonl", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".sh", ".bash", ".zsh", ".sql", ".css", ".scss", ".html", ".htm", ".xml", ".csv", ".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".java", ".kt", ".rb", ".php", ".swift", ".vue", ".svelte", ".log":
		return true
	}
	switch strings.ToLower(path.Base(file)) {
	case "dockerfile", "makefile", "gnumakefile", "license", "readme", ".gitignore", ".gitattributes", ".dockerignore", ".editorconfig":
		return true
	}
	return false
}

func nativeClientCall(req apiRequest, call, input object) (string, object, bool) {
	name := textValue(call["name"])
	if forced := textValue(field(req.ToolChoice, "function", "name")); req.ToolChoice == "none" || forced != "" && forced != name {
		return "", nil, false
	}
	if textValue(call["id"]) == "" || input == nil || !nativeToolName(name) || nativeEscalation(call) {
		return "", nil, false
	}
	args := object{}
	for key, value := range input {
		// Observed Work defaults, not client arguments. These are the only
		// normalized fields; retain the untouched native input for its reply.
		// Thumbnail hints have no effect on the known-text Read route.
		if name == "Read" && key == "thumbnail_size" && value == "full" {
			continue
		}
		// Default native sandbox selection does not override the HTTP
		// caller's own execution policy. Escalation is never translated.
		if name == "Bash" && key == "sandbox_permissions" && value == "use_default" {
			continue
		}
		if !nativeArgumentKey(name, key) {
			return "", nil, false
		}
		args[key] = value
	}
	if !nativeArgumentsKeepMeaning(name, args) {
		return "", nil, false
	}
	for _, tool := range req.Tools {
		if tool.Function.Name != name || !nativeCompatibleTool(tool) {
			continue
		}
		schema, err := compiledToolSchema(tool)
		// Validate canonical JSON values, including json.Number versus int.
		if err == nil && schema.Validate(parsed(jsonText(args))) == nil {
			return name, args, true
		}
	}
	return "", nil, false
}

// The caller's schema is validated afterwards; these checks keep calls whose
// platform meaning could differ (relative paths, empty targets, sandbox
// escape, unknown Read formats) on MCP. A background Bash run forwards only
// when the caller's own schema declares run_in_background.
func nativeArgumentsKeepMeaning(name string, args object) bool {
	text := func(key string) string { s, _ := args[key].(string); return s }
	absolute := func(key string) bool { return strings.HasPrefix(text(key), "/") }
	_, hasPath := args["path"]
	switch name {
	case "Read":
		offset, ok := args["offset"]
		return absolute("file_path") && nativeTextPath(text("file_path")) && (!ok || number(offset) >= 1)
	case "Write":
		_, ok := args["content"].(string)
		return absolute("file_path") && ok
	case "Edit":
		_, ok := args["new_string"].(string)
		return absolute("file_path") && text("old_string") != "" && ok
	case "NotebookEdit":
		return absolute("notebook_path")
	case "Bash":
		return strings.TrimSpace(text("command")) != "" && args["dangerouslyDisableSandbox"] != true
	case "Glob", "Grep":
		return text("pattern") != "" && (!hasPath || absolute("path"))
	case "TaskOutput", "TaskStop":
		// Only IDs the caller returned; shell_id is the caller's own alias.
		ids := 0
		for _, key := range []string{"task_id", "shell_id"} {
			if _, ok := args[key]; ok {
				if text(key) == "" {
					return false
				}
				ids++
			}
		}
		return ids > 0
	}
	return false
}

// Work moves a sandbox escape request into the call option. Any request other
// than the default sandbox changes the caller's policy and is never forwarded.
func nativeEscalation(call object) bool {
	option, _ := parsed(call["option"]).(map[string]any)
	escape, ok := option["bash_sandbox_escape"]
	if !ok || escape == nil || escape == "" {
		return false
	}
	settings, ok := parsed(escape).(map[string]any)
	permission := textValue(settings["sandbox_permissions"])
	return !ok || permission != "" && permission != "use_default"
}

func nativeClientInstructions(req apiRequest, connector string) string {
	docs := []string{}
	for _, tool := range req.Tools {
		if nativeCompatibleTool(tool) && nativeToolChoice(req, tool.Function.Name) {
			docs = append(docs, tool.Function.Name+"\n"+tool.Function.Description+"\n"+plainJSON(tool.Function.Parameters))
		}
	}
	if len(docs) == 0 {
		return ""
	}
	return "\n\n原生工具转发：下面明确列出的同名工具在满足兼容条件且属于单次调用或依赖链时，优先直接使用平台同名原生工具，无需 tool_search；多个独立调用必须优先使用上面的 MCP 并行批入口（如果本轮允许并行），不要为使用原生工具而把独立调用逐个串行执行；适配器会把结构化调用转交给 HTTP 调用方，在调用方环境中真实执行，然后回传实际结果。不是在平台机器上执行。参数必须严格符合下面调用方的完整说明与 schema，不得改变路径、内容、命令、模式或执行参数，也不要加入 schema 之外的平台参数。兼容范围：绝对路径的已知文本 Read（包括 .txt/.go/.py/.js/.ts/.md/.json/.yaml/.sh 等代码/文本文件及 Dockerfile、Makefile、.gitignore 等文本名；file_path、可选 offset/limit，offset 从 1 开始）；文本 Write（file_path、content）；Edit（file_path、非空 old_string、new_string、可选 replace_all）；NotebookEdit（绝对 notebook_path，其余参数按调用方 schema）；Glob、Grep（非空 pattern，path 省略或为绝对路径，不加排序、数量上限、目录或符号链接等平台参数，Grep 的 include 不会改写为 glob）；Bash（command、可选 description/timeout/run_in_background，timeout 单位毫秒；仅当调用方 schema 声明 run_in_background 时可后台运行）；TaskOutput、TaskStop（只用于调用方工具结果中返回的任务 ID，字段名按调用方 schema）。下文未列出 TaskOutput 时，按调用方 Bash 说明或其结果中给出的方式获取后台输出，不要调用未列出的工具。Write/Edit/NotebookEdit 的路径必须为绝对路径；执行前仍须遵守调用方完整说明中的读取、审批和修改约束，不得绕过客户端拒绝或重复执行失败的修改。平台 Read 默认 thumbnail_size=full 对文本无效，转发时仅移除此显示字段；Bash 默认 sandbox_permissions=use_default 不覆盖客户端权限策略，仅移除此默认字段。除此之外不丢弃任何字段。PDF/媒体、未知 Read 文件类型、提权（dangerouslyDisableSandbox 或 require_escalated）及其他不兼容参数仍使用 MCP 连接器。其他函数始终使用上面的 MCP 完整名。依赖调用必须等待真实结果，不能猜测或模拟输出。\n" + strings.Join(docs, "\n\n") + "\n可回退的 MCP 连接器：" + connector
}

func nativeToolChoice(req apiRequest, name string) bool {
	if req.ToolChoice == "none" {
		return false
	}
	forced := textValue(field(req.ToolChoice, "function", "name"))
	return forced == "" || forced == name
}

// Keep the native docs additive only when they fit: no valid MCP history or
// schema may be truncated just to enable the faster route.
func (g *gateway) renderToolPrompt(t *gatewayTurn, req apiRequest, connector string, continued bool) (string, error) {
	prompt := renderAPIRoutedPrompt(req, connector, continued, t.nativeTools)
	if t.nativeTools && utf16Len(prompt) > maxPromptChars {
		fallback := renderAPIPrompt(req, connector, continued)
		if utf16Len(fallback) <= maxPromptChars {
			g.mu.Lock()
			t.nativeTools = false
			err := g.checkpointLocked(t)
			g.mu.Unlock()
			if err != nil {
				return "", err
			}
			prompt = fallback
			timing(t, "native_prompt_fallback", time.Time{})
		}
	}
	if utf16Len(prompt) > maxPromptChars {
		return "", promptTooLong(prompt)
	}
	return prompt, nil
}
