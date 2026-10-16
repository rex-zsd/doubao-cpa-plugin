# Doubao Work native plugin for CLIProxyAPI

[GitHub](https://github.com/rex-zsd/doubao-cpa-plugin) · [中文文档](README_CN.md) · [Model names](docs/MODEL_CATALOG_CN.md) · [Validation](docs/VALIDATION_CN.md)

This plugin turns Doubao Work into a model provider for CLIProxyAPI (CPA). OpenAI Chat Completions and Responses clients call it directly, and Claude and Gemini clients work through CPA's translation. Client functions reach Work through a real MCP connector.

It is an independent Go protocol implementation that CPA loads as a `.so` through its C ABI. It neither needs nor runs doubao-cli and starts no extra gateway process. Only Linux amd64 and a single account are supported. Version 0.3.7 is tested on the official CPA v8.0.13.

Doubao Work runs an agent, and the plugin turns its conversation and its real connector calls into an LLM API. Low-level model controls such as system-message priority, temperature and token budgets are therefore not available.

## Install on CPA v8

The plugin runs on Linux amd64 with glibc 2.34 or newer. Building needs Go 1.26, CGO and a C compiler:

```bash
git clone https://github.com/rex-zsd/doubao-cpa-plugin.git
cd doubao-cpa-plugin
./scripts/build.sh /absolute/build-output
```

The build runs the race tests first and produces `doubao-work.so` and a static `doubao-helper`. Put both into CPA's plugin directory. The `.so` name must carry its version, because CPA loads the highest `<id>-v<version>.so` of each plugin:

```text
plugins/linux/amd64/doubao-work-v0.3.7.so
plugins/doubao-work/bin/doubao-helper
```

Add this to `config.yaml`, with the paths the CPA process (inside the container) sees:

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    doubao-work:
      enabled: true
      state_dir: /CLIProxyAPI/plugins/doubao-work/state
      helper_path: /CLIProxyAPI/plugins/doubao-work/bin/doubao-helper
```

Log in by QR code with the helper, which can run directly on the host:

```bash
/path/to/cpa/plugins/doubao-work/bin/doubao-helper -action login \
  -dir /path/to/cpa/plugins/doubao-work/state \
  -out /path/to/cpa/plugins/doubao-work/state/login-evidence
```

Scan `state/login-qr.png` with the Doubao app and wait until `state/login-status.json` shows `status=verified`. The plugin switches to the new session when idle; CPA does not need a restart.

To upgrade, copy the new versioned `.so` into the same directory and append a comment line to `config.yaml`. The hot reload switches to the highest version; then move the old file out.

## Models

Model IDs are slugs of the Work display names, such as `doubao-auto`, `doubao-2.1-pro`, `doubao-gpt-6.1-sol` and `doubao-orange-5.5`. `/v1/models` lists the ones your account offers. The old `doubao-work-<number>` and `doubao-work-pro` names now return `model_not_found`.

## What works

- `/v1/chat/completions` and `/v1/responses`, streaming or not, with the full visible history on every request. Claude `/v1/messages` and Gemini requests work through CPA.
- Up to 128 client functions, including Codex Responses Lite `additional_tools`, namespaced tools and custom tools. With `parallel_tool_calls=true` the model can request 1–8 independent calls in one response.
- Several tool-using requests at the same time, up to 4 active tasks.
- `reasoning_effort` selects Work's thinking level. `response_format` and forced `tool_choice` become prompt requirements.
- Image and file inputs, media in tool results, restart recovery of pending calls and `Idempotency-Key`.

## Limits

- Work accepts one message of at most 262,144 characters, so `/v1/models` reports a 65,536-token context. Longer requests get 400 `context_length_exceeded` with the `prompt is too long` wording that makes Claude Code and Codex compact.
- `temperature`, `max_tokens`, `stop` and similar sampling parameters are accepted and ignored. System messages are kept as a labeled transcript without native priority. Usage is an estimate (header `X-Doubao-Usage: estimated`) and not fit for billing.
- Work silently drops a connector tool whose description exceeds about 33,500 characters, and reads a tool's description plus schema reliably only up to about 66,000 characters. The plugin moves longer parts into the prompt.
- Work offers connector tools to its model only through its own `tool_search`, which returns three tools per query, under names like `mcp__independent_http_tools_<id>__get_weather` (lowercased, `-` becomes `_`). A search by the full name returned the tool first. Searched by several names at once, by a name spelled differently from Work's (such as `read-File`) or by a description, it returned only Work's built-in tools, and the model answered without calling the client function. Since 0.3.7 the prompt lists each function's full name and asks for one search per full name. Setting `tool_choice` to `required` or to a named function adds a further prompt requirement.
- There is no `/v1/files` endpoint and no `previous_response_id`. Use inline data, HTTPS URLs or helper-managed file IDs.

For Claude Code, point `ANTHROPIC_BASE_URL` at CPA and set `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `CLAUDE_CODE_MAX_CONTEXT_TOKENS=90000` and `CLAUDE_STREAM_IDLE_TIMEOUT_MS=900000`. [README_CN.md](README_CN.md) covers these settings, the error codes and the tested limits in detail.

Account sessions, request receipts and tool results stay in the private state directory. Licensing and source acknowledgments are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
