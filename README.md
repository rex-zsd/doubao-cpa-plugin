# Doubao Work native plugin for CLIProxyAPI

[GitHub](https://github.com/rex-zsd/doubao-cpa-plugin) · [中文文档](README_CN.md) · [Model names](docs/MODEL_CATALOG_CN.md) · [Validation](docs/VALIDATION_CN.md)

This plugin turns Doubao Work into a model provider for CLIProxyAPI (CPA). OpenAI Chat Completions and Responses clients call it directly, and Claude and Gemini clients work through CPA's translation. Client functions reach Work through a real MCP connector.

It is an independent Go protocol implementation that CPA loads as a `.so` through its C ABI. It neither needs nor runs doubao-cli and starts no extra gateway process. Only Linux amd64 and a single account are supported. Version 0.3.24 is tested on the official CPA v8.0.13; see the [native tool prompt report](docs/NATIVE_PROMPT_0324_CN.md).

CPA OAuth model aliases work with native Chat Completions and Responses requests as well as translated clients. See the [model alias notes](docs/MODEL_ALIASES_CN.md) for a Claude client compatibility example.

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
plugins/linux/amd64/doubao-work-v0.3.29.so
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
      native_tools: false # Optional compatible native forwarding, described below.
```

Log in by QR code with the helper, which can run directly on the host:

```bash
/path/to/cpa/plugins/doubao-work/bin/doubao-helper -action login \
  -dir /path/to/cpa/plugins/doubao-work/state \
  -out /path/to/cpa/plugins/doubao-work/state/login-evidence
```

Scan `state/login-qr.png` with the Doubao app and wait until `state/login-status.json` shows `status=verified`. The plugin switches to the new session when idle; CPA does not need a restart. Since 0.3.22 you can also log in from the plugin page.

Since 0.3.22 the plugin adds a page to CPA's management panel (plugin menu "豆包 Work", or `/v0/resource/plugins/doubao-work/console`). CPA serves plugin pages without authentication, so the page holds no data: it asks for the CPA management key and reads everything through routes under `/v0/management/doubao-work/`, which need that key like any other management route. It shows the login state with QR login, running and recent tasks (model, mode, the client function a task waits on, time left) with cancel, 24-hour outcomes by cause, resumed streams and merged re-issues, and a one-question test with timings. The routes return metadata only, never prompts, tool arguments, results, answers or the session.

To upgrade, copy the new versioned `.so` into the same directory and append a comment line to `config.yaml`. The hot reload switches to the highest version; then move the old file out.

## Models

Model IDs are slugs of the Work display names, such as `doubao-auto`, `doubao-2.1-pro`, `doubao-gpt-6.1-sol` and `doubao-orange-5.5`. `/v1/models` lists the ones your account offers. The old `doubao-work-<number>` and `doubao-work-pro` names now return `model_not_found`.

## What works

- `/v1/chat/completions` and `/v1/responses`, streaming or not, with the full visible history on every request. Claude `/v1/messages` and Gemini requests work through CPA.
- Requests without tools use Doubao's ordinary chat mode and fall back to Work when chat mode refuses them. Requests with tools run in Work on the local-computer runtime.
- A multi-turn conversation stays in one Doubao conversation. When a request's history is exactly an earlier answered history plus that answer plus new messages, the plugin continues that conversation and sends only the new messages. Otherwise it opens a new conversation with the full transcript: after an edited or compacted history, a different model or tool list, a repeated history (regenerate or concurrent requests), 12 idle hours, more than 128 open conversations (the least recently used goes first) or a CPA restart, and when Doubao refuses a continuation before accepting it.
- Up to 128 client functions, including Codex Responses Lite `additional_tools`, namespaced tools and custom tools. With `parallel_tool_calls=true` the model can request 1–8 independent calls in one response.
- Several tool-using requests at the same time, up to 4 active tasks.
- `reasoning_effort` selects Work's thinking level. `response_format` and forced `tool_choice` become prompt requirements.
- Image and file inputs, media in tool results, restart recovery of pending calls and `Idempotency-Key`.

## Limits

- Work accepts one message of at most 262,144 characters, so `/v1/models` reports a 65,536-token context. A continuation sends only the new messages, but the limit still applies to the full transcript, which is resent if the continuation is refused. Longer requests get 400 `context_length_exceeded` with the `prompt is too long` wording that makes Claude Code and Codex compact.
- `temperature`, `max_tokens`, `stop` and similar sampling parameters are accepted and ignored. System messages are kept as a labeled transcript without native priority. Usage is an estimate (header `X-Doubao-Usage: estimated`) and not fit for billing.
- Work silently drops a connector tool whose description exceeds about 33,500 characters, and reads a tool's description plus schema reliably only up to about 66,000 characters. The plugin moves longer parts into the prompt.
- Work offers connector tools to its model only through its own `tool_search`, which returns three tools per query, under names like `mcp__independent_http_tools_<id>__get_weather` (lowercased, `-` becomes `_`). A search by the full name returned the tool first. Searched by several names at once, by a name spelled differently from Work's (such as `read-File`) or by a description, it returned only Work's built-in tools, and the model answered without calling the client function. Since 0.3.7 the prompt lists each function's full name and asks for one search per full name. Setting `tool_choice` to `required` or to a named function adds a further prompt requirement.
- Work itself waits about a minute for each client function. Measured on 2026-10-17 with doubao-2.1-pro: results returned after 45 seconds were fine. When results came back after 120 or 170 seconds, Work issued the same call again (same arguments, new call ID) 66–96 seconds after it last issued it, once or twice, then ended the task on its own, answering that the call had failed and been retried. Up to 0.3.21 the plugin passed each repeat to the client as soon as the earlier result arrived, so the same function ran two or three times. Since 0.3.22 a repeat that arrives while the original call is still unanswered reuses the original result and error flag and never reaches the client; an identical call that arrives after the answer is a new call and runs again. Measured on 2026-10-25 (UTC) with Orange 5.5: Work repeated a call after 65 seconds, the plugin merged it and the client saw one call. In another run Work stopped waiting about 130 seconds after a call that got no result and answered without it. A result that arrives after the task ended starts a new task over the full transcript. Return function results within a minute and run slower commands in the background. In Claude Code this includes the time a permission prompt waits.
- There is no `/v1/files` endpoint and no `previous_response_id`. Use inline data, HTTPS URLs or helper-managed file IDs.

For Claude Code, point `ANTHROPIC_BASE_URL` at CPA and set `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `CLAUDE_CODE_MAX_CONTEXT_TOKENS=90000` and `CLAUDE_STREAM_IDLE_TIMEOUT_MS=900000`. [README_CN.md](README_CN.md) covers these settings, the error codes and the tested limits in detail.

Account sessions, request receipts and tool results stay in the private state directory. Licensing and source acknowledgments are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Version 0.3.17 preserves Claude tool-result error flags through upload and recovery, restores explicit Claude parallel permissions, and keeps full original definitions beside grouped, locally validated batch entries with complete schemas for large catalogs (scoped or oversized schemas use a separate compact entry). Explicitly disabled parallel calls remain disabled. Continued conversations are checked against the actual outgoing delta; an oversized full-history fallback fails with a compaction error instead of truncating history. Additional timing spans separate native result upload, stream completion and verified readback.

Version 0.3.18 adds persistent connector leases in the CPA plugin, keyed by account, device, helper and the entire immutable tool catalog. Up to eight catalogs remain cached for ten idle minutes; availability is checked on every acquisition. Tasks retain separate sandboxes, calls and results. Shutdown joins task workers before draining idle entries and preserves suspended live leases. A full cache falls back to the existing per-task connector path. Reuse targets registration overhead only; it does not guarantee a fixed end-to-end speedup. Timing also identifies the first upstream chunk and timeout-control events without logging content.

Version 0.3.19 adds opt-in `native_tools: true`. Compatible single calls and dependency chains can use Work's native channel without MCP discovery: explicitly declared, same-name `Read` for absolute `.txt` paths (optional one-based offset and limit), and foreground `Bash` with the original command and millisecond timeout. The HTTP client still executes every call and returns its actual result. Arguments must pass the original client schema. Only observed native display/sandbox defaults are removed; native IDs and original inputs are retained for replies. Media, other file types, background/elevated commands and all other tools retain MCP. Complete tools, descriptions, schemas and history remain available; independent calls prefer existing MCP batches when parallelism is allowed. If the added native instructions exceed the context limit, the task uses the complete MCP prompt when it fits. Pending calls restore their persisted routing policy even after configuration changes. The flag defaults to false; apply changes through a plugin version reload or a clean stop/reload. This reduces routing overhead for compatible calls while Work remains an Agent; it does not guarantee a fixed end-to-end speedup.

Version 0.3.20 moves registration, availability checks and retirement outside the global connector-pool lock. Operations for the same catalog coordinate with cancellable waits; different catalogs can progress independently. Durable leases, creation/disconnect intents, capacity limits and recovery remain in place. Native forwarding also accepts known code/text Read paths (including `.go`, `.py`, `.js`, `.ts`, `.md` and `.json`) and same-name text Write/Edit. The client retains execution and approval; original arguments, actual results and errors survive HTTP replay and recovery. Unknown Read formats and incompatible arguments retain MCP. These changes target routing overhead and contention, with the same model, effort, full history and tool definitions.

Version 0.3.29 reports Work's refusals by kind, as Doubao's own CLI sorts them. A refusal for an expired login is 401 `login_required`; one for a busy or rate-limited Work is 429 `upstream_busy`, which clients retry, since nothing ran; a malformed request is 400 and a feature the account may not use is 403. Other codes remain 422 `upstream_rejected`. Expired-login and busy refusals no longer fall back to a fresh conversation or another mode, which would only be refused again. A reply that Work withheld under its content policy now fails at once with 422 `content_blocked` instead of waiting for the task deadline, and a reply Work marks as ended in error or timeout fails as well.

Version 0.3.27 trims two waits on every task. A cached connector that Work confirmed, or that a task used without failing, in the last two minutes is reused without listing the account's connectors again; that listing took about a second at the start of each task. After the stream ends, the final answer is read back every 250 ms for the first two seconds instead of every second.

Version 0.3.26 reports Work's own context size as `prompt_tokens`. Each finished answer carries how many tokens the system prompt, tools, skills and messages took in Work's model context; the plugin already reads that answer back, so this costs no extra request. Clients that size their context from usage, such as Claude Code, now see Work's overhead (about 7,000 to 42,000 tokens depending on the model) instead of a character-based estimate of their own prompt. Tool calls in the middle of a task report at least the figure from the conversation's previous answer. The estimate remains the fallback when Work reports nothing. Completion tokens are still estimated.

Version 0.3.25 reloads Work's model list every hour on its own and, when models were added or retired, rewrites the account's refresh mark so that CPA registers the new list. Retired models such as Orange 5.5 leave `/v1/models` without a reload or a click on the plugin page.

Version 0.3.24 stops repeating the full description and schema of each native-compatible tool in every prompt. Work's own tools take the same names and arguments, so the prompt lists the declared tools with the parameters the native route forwards and states only where the platform differs, about 8,000 characters less per new task with a 32-tool Claude Code catalog. The connector keeps the complete definitions, and every native call is still validated against the caller's schema, falling back to MCP when it does not fit.

Version 0.3.23 extends the plugin page. It refreshes the model list and makes CPA register it again without a plugin reload. It cleans up what the plugin leaves in the account: conversations the plugin recorded itself from 0.3.23 on (Work lists plugin conversations exactly like the user's own, so older ones are left alone; conversations a task or a continuable thread uses are kept), idle plugin connectors recognized by their name prefix (only while no task runs; never the user's own), and files uploaded through the plugin's file API. Plugin configuration edited in CPA's panel now applies without a failed reconfigure: `native_tools` at once for new tasks, `state_dir` and `helper_path` once no task runs.

Version 0.3.22 merges Work re-issues of unanswered client calls (above), and no longer fails a task whose response stream drops after Work acknowledged it: an async task stream is subscribed again from its saved cursor, otherwise the run is followed by readback until the final answer, and a dropped stream never submits another generation (a drop before acknowledgement still fails, as acceptance is unknown). Receipts are written on state changes and at most once a second for cursor progress instead of after every event. Running tasks can be cancelled from the plugin page; a tool result sent for a cancelled task starts a new task over the full transcript instead of failing with 409.

Version 0.3.21 extends native forwarding to same-name `NotebookEdit`, `Glob`, `Grep`, `TaskOutput` and `TaskStop`, and to background `Bash` when the client's own Bash schema declares `run_in_background`. A tool forwards only when the client declares it with a compatible schema and the unchanged call passes that schema: absolute paths, non-empty patterns, no Work-only Glob options (ordering, result limits, entry kinds) in the arguments and no alias translation (such as Grep `include` or TaskOutput `shell_id`). The `glob_config` that Work attaches to Glob call options holds fixed defaults, not model intent, and is ignored. Sandbox escalation is never forwarded, whether it appears in the arguments or in Work's call options. Undeclared or incompatible tools, for example Glob/Grep/TaskOutput for clients that do not declare them, keep the existing MCP route hint; the reply format for unsupported tools is unchanged.
