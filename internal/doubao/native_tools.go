package doubao

import (
	"path"
	"strings"
	"time"
)

func nativeBuiltinsRule(probe bool) string {
	if probe {
		return "只有下文原生工具转发明确列出的同名 Read/Bash/Edit/Write 可直接调用；未列出或参数不兼容的工具必须使用调用方 MCP 函数。其他平台内置工具不可用；同名但未列出的 Read/Bash/Edit/Write 也须走 MCP。"
	}
	return "不要改用你所在平台自带的 Bash、Read、Write、Edit、computer_use_tool 等内置工具代替这些函数；这些内置工具在此环境不可用，重试、更换权限或通过 Python subprocess 调用也不会成功。这是工具路由的区别，不影响调用方 MCP 函数的可用性。"
}

func nativeDiscoveryRule(probe bool) string {
	if probe {
		return "单次或依赖链中，下文原生转发范围内的 Read/Bash/Edit/Write 优先直接调用；独立调用优先 MCP 并行批入口。其他函数使用前，先调用"
	}
	return "使用其中一个函数前，先调用"
}

// This is a deliberately narrow compatibility route. A matching name is
// insufficient: preserve the caller's schema, arguments, descriptions, execution
// environment and actual results. No native shell or reader runs in this helper.
func nativeCompatibleTool(tool apiTool) bool {
	name := tool.Function.Name
	if name != "Read" && name != "Bash" && name != "Edit" && name != "Write" {
		return false
	}
	schema := tool.Function.Parameters
	required, properties := "file_path", field(schema, "properties")
	if name == "Bash" {
		required = "command"
	}
	if schema["type"] != "object" || field(properties, required, "type") != "string" {
		return false
	}
	if name == "Write" && field(properties, "content", "type") != "string" || name == "Edit" && (field(properties, "old_string", "type") != "string" || field(properties, "new_string", "type") != "string") {
		return false
	}
	for _, key := range array(parsed(jsonText(schema["required"]))) {
		if !nativeArgumentKey(name, textValue(key)) {
			return false
		}
	}
	compiled, err := compiledToolSchema(tool)
	return err == nil && compiled != nil
}

func nativeArgumentKey(name, key string) bool {
	switch name {
	case "Read":
		return key == "file_path" || key == "offset" || key == "limit"
	case "Write":
		return key == "file_path" || key == "content"
	case "Edit":
		return key == "file_path" || key == "old_string" || key == "new_string" || key == "replace_all"
	case "Bash":
		return key == "command" || key == "description" || key == "timeout" || key == "run_in_background" || key == "dangerouslyDisableSandbox"
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
	if textValue(call["id"]) == "" || input == nil || name != "Read" && name != "Bash" && name != "Edit" && name != "Write" {
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
	if name != "Bash" {
		file, ok := args["file_path"].(string)
		if !ok || !strings.HasPrefix(file, "/") || name == "Read" && !nativeTextPath(file) {
			return "", nil, false
		}
		if offset, ok := args["offset"]; name == "Read" && ok && number(offset) < 1 {
			return "", nil, false
		}
		if name == "Write" {
			if _, ok := args["content"].(string); !ok {
				return "", nil, false
			}
		}
		if name == "Edit" {
			old, ok := args["old_string"].(string)
			if !ok || old == "" {
				return "", nil, false
			}
			if _, ok := args["new_string"].(string); !ok {
				return "", nil, false
			}
		}
	} else if command, ok := args["command"].(string); !ok || strings.TrimSpace(command) == "" || args["run_in_background"] == true || args["dangerouslyDisableSandbox"] == true {
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
	return "\n\n原生工具转发：下面明确列出的 Read/Bash/Edit/Write 在满足兼容条件且属于单次调用或依赖链时，优先直接使用平台同名原生工具，无需 tool_search；多个独立调用必须优先使用上面的 MCP 并行批入口（如果本轮允许并行），不要为使用原生工具而把独立调用逐个串行执行；适配器会把结构化调用转交给 HTTP 调用方，在调用方环境中真实执行，然后回传实际结果。不是在平台机器上执行。参数必须严格符合下面调用方的完整说明与 schema；支持绝对路径的已知文本 Read（包括 .txt/.go/.py/.js/.ts/.md/.json/.yaml/.sh 等代码/文本文件及 Dockerfile、Makefile、.gitignore 等文本名；file_path、可选 offset/limit，offset 从 1 开始）、文本 Write（file_path、content）、Edit（file_path、非空 old_string、new_string、可选 replace_all），以及前台 Bash（command、可选 description/timeout，timeout 单位毫秒），不得改变路径、内容、命令或执行参数。Write/Edit 的路径也必须为绝对路径；执行前仍须遵守调用方完整说明中的读取、审批和修改约束，不得绕过客户端拒绝或重复执行失败的修改。平台 Read 默认 thumbnail_size=full 对文本无效，转发时仅移除此显示字段；Bash 默认 sandbox_permissions=use_default 不覆盖客户端权限策略，仅移除此默认字段。除此之外不丢弃任何字段。PDF/媒体、未知 Read 文件类型、后台运行、提权及其他不兼容参数仍使用 MCP 连接器。其他函数始终使用上面的 MCP 完整名。依赖调用必须等待真实结果，不能猜测或模拟输出。\n" + strings.Join(docs, "\n\n") + "\n可回退的 MCP 连接器：" + connector
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
