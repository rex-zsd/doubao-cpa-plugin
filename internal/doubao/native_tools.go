package doubao

import (
	"path"
	"sort"
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
	tools := []string{}
	for _, tool := range req.Tools {
		if nativeCompatibleTool(tool) && nativeToolChoice(req, tool.Function.Name) {
			tools = append(tools, tool.Function.Name+"("+nativeParameters(tool)+")")
		}
	}
	if len(tools) == 0 {
		return ""
	}
	// Work's own tools carry the same names and arguments as these, so the
	// prompt names the caller's tools and parameters and states only where
	// they differ; every call is still checked against the caller's schema
	// and falls back to MCP when it does not fit.
	return "\n\n原生工具转发：调用方声明了与平台自带工具同名的 " + strings.Join(tools, "、") + "（括号内为调用方的参数名，* 为必填）。单次调用或依赖链中直接使用平台同名工具，无需 tool_search；多个独立调用必须优先使用上面的 MCP 并行批入口（如果本轮允许并行），不要为使用原生工具而把独立调用逐个串行执行。这些调用会转交 HTTP 调用方在它自己的环境中真实执行并回传实际结果，不在平台机器上执行；调用方的审批和修改约束同样有效，不得绕过拒绝或重复执行失败的修改。只使用括号里的参数，含义与平台同名工具相同，不要加入平台独有参数。路径一律为绝对路径；Read 只用于文本和代码文件，offset 从 1 开始；Bash 的 timeout 单位为毫秒，只有括号里有 run_in_background 时才能后台运行；Glob 不加排序、数量上限、目录或符号链接选项；Grep 不用 include，按括号里的参数名传；TaskOutput、TaskStop 只用于调用方结果中返回的任务 ID，括号里没有 TaskOutput 时按调用方 Bash 结果给出的方式获取后台输出；不得使用沙箱逃逸或提权参数。PDF、图片等媒体和其他不兼容的调用，改用 MCP 连接器里的同名函数。其他函数始终使用上面的 MCP 完整名。依赖调用必须等待真实结果，不能猜测或模拟输出。\n可回退的 MCP 连接器：" + connector
}

// nativeParameters lists the declared parameters the native route forwards,
// required ones first and marked with *, so calls use the caller's names.
func nativeParameters(tool apiTool) string {
	properties, _ := field(tool.Function.Parameters, "properties").(map[string]any)
	required := map[string]bool{}
	for _, key := range array(parsed(jsonText(tool.Function.Parameters["required"]))) {
		required[textValue(key)] = true
	}
	names := []string{}
	for key := range properties {
		// Only arguments the native route forwards; sandbox escape never is.
		if nativeArgumentKey(tool.Function.Name, key) && key != "dangerouslyDisableSandbox" {
			names = append(names, key)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	for i, key := range names {
		if required[key] {
			names[i] += "*"
		}
	}
	return strings.Join(names, ", ")
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
