# 豆包 Work · CLIProxyAPI 原生插件

把豆包 Work 接成 CLIProxyAPI（CPA）的模型提供方。OpenAI Chat Completions / Responses 客户端可以直接调用；Claude、Gemini 客户端经 CPA 转换后也能用。客户端函数通过真实 MCP 连接器交给 Work 调用。

插件是独立的 Go 协议实现，通过 CPA 的 C ABI 加载 `.so`。它不依赖也不执行 doubao-cli，也不需要另起网关进程。目前只支持 Linux amd64、单账号。

豆包 Work 实际运行的是 agent，插件把它的对话和真实连接器调用转换成 LLM API。系统消息优先级、温度、token 预算等底层模型语义无法因此获得，见下文的参数表。

当前版本为 0.3.9，已在官方 CPA v8.0.13 上实测。

## 安装到 CPA v8

运行依赖：Linux amd64，glibc 2.34 或更新。构建需要 Go 1.26、CGO 和 C 编译器：

```bash
git clone https://github.com/rex-zsd/doubao-cpa-plugin.git
cd doubao-cpa-plugin
./scripts/build.sh /absolute/build-output
```

构建前会先跑 race 回归。完成后得到 `doubao-work.so` 和静态链接的 `doubao-helper`。

把这两个文件放进 CPA 的插件目录。`.so` 的文件名要带版本号：CPA 按 `<id>-v<版本>.so` 识别插件，同一插件有多个版本时加载最高的那个。

```text
plugins/linux/amd64/doubao-work-v0.3.9.so
plugins/doubao-work/bin/doubao-helper
```

在 `config.yaml` 中加入以下配置，路径写 CPA 进程（容器内）看到的路径：

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

扫码登录用 helper 完成。helper 是静态二进制，可以直接在宿主机上运行，`-dir` 写 state 目录在宿主机上的路径：

```bash
/path/to/cpa/plugins/doubao-work/bin/doubao-helper -action login \
  -dir /path/to/cpa/plugins/doubao-work/state \
  -out /path/to/cpa/plugins/doubao-work/state/login-evidence
```

用豆包客户端扫描 `state/login-qr.png` 并确认，等 `state/login-status.json` 显示 `status=verified`。插件会在空闲时自动换用新会话，不需要重启 CPA。账户凭据只写在私有的 state 目录。

升级时，把新版本的 `.so` 复制进同一目录。CPA 配置热重载后会切换到最高版本，在 `config.yaml` 末尾追加一行注释即可触发热重载。确认切换后，把旧版本文件移出插件目录。

管理页面的管理密钥、模型 API key 和豆包登录会话三者用途不同，`missing management key` 的排查见 `docs/AUTH_TROUBLESHOOTING_CN.md`。

## 模型

插件在首次打开服务时读取当前账号的 Work 模型清单，只注册可用且支持 Work 模式的项。显示名会转成固定的 slug：去掉“豆包”，转小写，字母、数字和点以外的字符换成 `-`，再加上 `doubao-` 前缀。“自动”对应 `doubao-auto`，重名时追加上游模型 key。

| Work 显示名 | model |
|---|---|
| 自动 | `doubao-auto` |
| 豆包 2.1 Pro | `doubao-2.1-pro` |
| 豆包 2.1 Turbo | `doubao-2.1-turbo` |
| 豆包 2.1 Lite | `doubao-2.1-lite` |
| GPT-6.1 Sol | `doubao-gpt-6.1-sol` |
| GPT-6 Astra | `doubao-gpt-6-astra` |
| GPT-5.6 Sol | `doubao-gpt-5.6-sol` |
| Gemini 3.7 Flash | `doubao-gemini-3.7-flash` |
| Orange 5.5 | `doubao-orange-5.5` |

上表是 2026-10-16 测试账号的清单，会随账号权限和上游配置变化，以 `/v1/models` 为准。旧版的 `doubao-work-<数字>` 和 `doubao-work-pro` 已不再注册，用它们请求会返回 `model_not_found`。清单排查见 `docs/MODEL_CATALOG_CN.md`。

Work 每次只接收一条消息，最多 262,144 个字符（UTF-16 码元）。插件把完整对话、函数说明和格式要求都渲染进这一条消息；续聊时只发送新增的消息，但仍按完整对话计算上限，因为续聊被拒绝时要整段重发。所以 `/v1/models` 报告的上下文是 65,536 token，约等于 262,144 个英文字符。超长的请求返回 400 `context_length_exceeded`，消息采用 `prompt is too long: N tokens > M maximum` 格式，Claude Code 和 Codex 能据此自动压缩上下文。

## 接口与参数

支持 `/v1/chat/completions` 和 `/v1/responses`，非流式和流式都可以。Claude `/v1/messages` 和 Gemini 请求由 CPA 转换后进入插件。每次请求都必须携带完整的可见历史。不带函数的请求走豆包的普通对话模式，被拒绝时自动改用 Work；带函数的请求走 Work 的本地电脑模式。`instructions` 和 system/developer 消息会保留为带角色的对话记录，但不承诺原生的 system 优先级。

多轮对话会留在同一个豆包会话里：如果请求的历史恰好是插件回答过的某段历史，加上那次的回答，再加上新消息，插件就在原会话里续聊，只发送新增的消息。其余情况都开新会话并发送完整对话记录，包括：历史被改过、客户端压缩了上下文、换了模型或函数列表、同一段历史被再次请求（重新生成或并发请求）、会话闲置超过 12 小时、可续聊的会话超过 128 个（先丢最久未用的）、CPA 重启过。豆包在接收前拒绝续聊时，插件自动开新会话重发完整记录。

| 参数 | 处理方式 |
|---|---|
| `temperature`、`top_p`、`max_tokens`、`max_completion_tokens`、`max_output_tokens`、`stop`、`seed`、`presence_penalty`、`frequency_penalty`、`logprobs` 等 | 接受，但会被忽略。Work 没有这些开关（2026-10 实测） |
| `reasoning_effort`、`reasoning.effort` | `none`/`minimal`/`low`、`medium`、`high`、`xhigh`、`max` 依次对应更高的思考档位，也接受 1–100 的数字。取模型支持的、不超过所请求档位的最高一档 |
| `response_format`：`json_object`、`json_schema` | 转成提示词要求，并去掉回答外层的 Markdown 代码块。不做 strict 校验 |
| `tool_choice` | 支持 `auto` 和 `none`。`required` 和指定函数会转成提示词要求，因为 Work 不能强制调用。无法识别的对象按 `auto` 处理 |
| `parallel_tool_calls=true` | 通过 `client_tools_batch` 一次给出 1–8 个调用，见下节 |
| `stream_options.include_usage` | 支持。usage 一律是估算值：约 4 个 ASCII 字符或 1 个其他字符计为 1 token，响应头为 `X-Doubao-Usage: estimated`，不能用于计费 |
| `n` 不等于 1、音频输出、旧版 `functions`/`function_call` | 返回 400 |
| `previous_response_id`、后台 Responses 存储 | 不支持 |

Codex 的 Responses Lite 请求可以直接使用，包括 `additional_tools`、命名空间工具和 custom 工具。插件按 CPA 的规则把它们展开成函数名，例如 `functions__exec`。CPA 再把调用还原成带 `namespace` 的 `function_call`，custom 工具还原为 `custom_tool_call`。Gemini 客户端发来的 OpenAPI 风格大写类型名（`OBJECT`、`STRING` 等）会转成 JSON Schema。

## 函数调用

客户端函数通过真实 MCP 连接器提供给 Work。插件收到标准的 `tool_calls` / `function_call` 后交给客户端执行。客户端需要把原始 assistant 调用和 tool 结果一起回传，插件再把结果交回**原来的 Work 任务**。本地 shell、读写文件等未声明的工具不会被执行。

Work 不把连接器工具直接列给模型：模型要先用 Work 自带的 `tool_search` 搜索，搜到后再按 Work 的方式调用。连接器工具在 Work 里的名字形如 `mcp__independent_http_tools_<编号>__get_weather`（函数名转成小写，`-` 换成 `_`）。`tool_search` 每次只返回 3 个工具。2026-10-16 实测：用完整名字搜索时，连接器工具排在第一；只写 `Read`、`Bash` 这样的函数名时，它排在同名的 Work 自带工具之后；一次搜几个函数（如 `Read Bash`）、写法与 Work 的名字不同（如 `read-File`）或用“读取文件内容”这类描述来搜时，3 个结果全是 Work 自带的工具，模型找不到客户端函数，就直接回答了。0.3.5 及更早的提示词只给出连接器名，13–20 个函数时 7 次里有 2 次这样失败；0.3.6 加上函数名后，Claude Code 3 次里仍有 2 次失败；0.3.7 起提示词列出每个函数的完整名字，并要求每次只按一个完整名字搜索（结果见[验收记录](docs/VALIDATION_CN.md)）。需要确保调用客户端函数时，可以再把 `tool_choice` 设为 `required` 或指定函数（同样是提示词要求，见上表）。

函数的限制：

- 最多 128 个函数。名称为 1–64 个字母、数字、`_` 或 `-`，不能重名。
- 说明（description）：Work 会静默丢弃说明过长的连接器工具。这个上限按字符数计算，与字节数和 token 数无关，而且针对单个函数：逐个提供时，20 个 5,000 字符、12 个 8,000 字符的说明都能全部读到。2026-10-16 实测 33,515 个字符的说明可用，39,995 个不可用。超过 32,000 个字符的说明在连接器里只保留开头约 2,000 个字符和一条指引，完整说明放进提示词，计入 262,144 字符的上限。
- 参数（parameters）：必须是 JSON Schema object，序列化后最多 1 MiB。Work 把一个函数的说明和参数结构合在一起读：2026-10-16 实测两者合计约 66,000 个字符时十秒左右就能读完；75,000–100,000 个字符时要等 4–6 分钟，而且只读到一部分；参数结构里单个 80,000 字符的字符串完全读不到。所以连接器里的说明（过长时为缩短版）加参数结构超过 64,000 个字符时，插件把完整的参数结构放进提示词，连接器里的参数改为接受任意对象，模型按提示词里的结构填写参数。

显式设置 `parallel_tool_calls=true` 时，连接器里只有一个 `client_tools_batch` 工具，模型在一次真实调用里选择 1–8 个相互独立的函数。插件把它们拆成多个 call_id，由客户端并行执行。一个批次的结果必须在同一个请求里全部提交，顺序可以不同；依赖前一批结果的调用应放进下一批。不用批处理时，Work 有时也会一次发起几个连接器调用，插件按顺序每次只交给客户端一个，前一个的结果交回后再交下一个。

`client_tools_batch` 的参数结构包含所有函数的说明和参数，和普通函数一样受上面的读取上限约束。为了留出余量，批处理结构超过 48,000 个字符时，插件不提供 `client_tools_batch`，改为逐个提供函数，模型一次调用一个，客户端不用做任何改动。2026-10-16 实测 13 个函数、51,200 个字符的批处理结构按此回退后正常完成。

Claude 客户端经 CPA 转换后不会带 `parallel_tool_calls=true`，Gemini 也不会，所以这两类客户端总是逐个调用函数。

多个带函数的请求可以同时进行。Work 把一个设备的连接器调用都从最新打开的工具通道发下来，不管调用属于哪个任务；插件的所有任务共用登录会话里的同一个设备，0.3.5 起会把每个调用转交给它所属的任务。0.3.4 及更早的版本会丢掉不属于本通道的调用：较早开始的任务要等较新的任务结束才拿得到调用，期间 Work 会说“连接器暂时不可用”并退避重试，任务因此慢上几分钟。同理，不要把 `session.json` 复制给另一个实例同时使用，否则两边的调用会发到对方的通道上。

首次请求建议带 `Idempotency-Key`（最多 128 个字符）。同一个 key 配相同的请求会复用原任务，配不同的请求返回 409。工具结果按 call_id 去重，结果冲突时返回 409。

## 错误与重试

- 429 `gateway_capacity`：同时最多运行 4 个任务。容量满时，先挤掉等待客户端函数结果最久的任务；它的客户端之后交回结果时，插件按完整历史开一个新任务继续。如果没有这样的任务，就拒绝新请求。被拒绝的请求没有提交任何内容，可以安全重试。
- 429 `task_not_started`：Work 没能启动任务，也没有提交任何内容，可以安全重试。
- 409 `upstream_failed`、504 `task_expired`：生成已经提交过，插件不会自动重发。请先核对私有收据，再决定是否重新请求。504 `task_expired` 表示豆包在一次等待里超过任务期限（默认 10 分钟）还没给出下一个函数调用或最终回答。
- 401：通常需要重新扫码。

插件给自己的账号关闭了 CPA 冷却，429 不会让模型被暂停。其他层的重试可能把一个不确定的写请求当成新请求重发，所以建议：SDK 设 `max_retries=0`，CPA 设 `request-retry=0`、`bootstrap-retries=0`、`max-retry-credentials=1`；或者让客户端带上 Idempotency-Key。

## 在 Claude Code 中使用

把 Claude Code 指向 CPA，并设置以下环境变量：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_AUTH_TOKEN=<CPA 的模型 API key>
export ANTHROPIC_MODEL=doubao-gpt-6.1-sol
export ANTHROPIC_DEFAULT_HAIKU_MODEL=doubao-2.1-lite
export CLAUDE_CODE_MAX_CONTEXT_TOKENS=90000
export CLAUDE_STREAM_IDLE_TIMEOUT_MS=900000
```

Work 只接收 262,144 个字符，限制上下文能让 Claude Code 及时压缩，不会撞到这个上限。Work 的单次回答可能要几分钟，较长的流式空闲超时可以避免 Claude Code 提前断开。

- 后台小任务的模型用 `ANTHROPIC_DEFAULT_HAIKU_MODEL` 指定，`ANTHROPIC_SMALL_FAST_MODEL` 已弃用。
- `CLAUDE_CODE_MAX_CONTEXT_TOKENS` 只对不以 `claude-` 开头的模型名生效。不设置时 Claude Code 按 200K 计算，要到远超 Work 上限时才会压缩。
- `CLAUDE_STREAM_IDLE_TIMEOUT_MS` 最小为 300000，超过 30 分钟的值按 30 分钟算。
- 万一请求仍然超长，插件返回的 `prompt is too long` 错误也会触发 Claude Code 自动压缩。
- 豆包只等每个函数约 1 分钟。权限确认等了 1 分钟以上，或者命令运行超过 1 分钟，同一个调用都可能再执行一次，见[生命周期与恢复](#生命周期与恢复)。

## 图片、文件和工具媒体结果

用户内容支持：

- Chat 的 `image_url` 和 `file` 内容块；Responses 的 `input_image`、`input_file`。
- base64 data URL；文件也可用 `filename` 加原始 base64。
- `file_url` / 图片 HTTPS URL：只允许公网 HTTPS 443，不跟随重定向，不携带账号 cookie；宿主验证解析地址并固定实际连接 IP，代理 CONNECT 也固定到该 IP，TLS 身份仍按原域名校验。
- 账户私有 `file_id`：通过 helper 注册本地文件，再放进 `file.file_id` 或 `input_file.file_id`。未注册、跨 state 目录或过期 ID 会报错。

```bash
/absolute/doubao-cpa/bin/doubao-helper -action file-put \
  -dir /absolute/doubao-cpa/state -file /absolute/report.pdf
/absolute/doubao-cpa/bin/doubao-helper -action file-list \
  -dir /absolute/doubao-cpa/state
/absolute/doubao-cpa/bin/doubao-helper -action file-delete \
  -dir /absolute/doubao-cpa/state -file-id file_返回的标识
```

`file_id` 是本插件的本地资源，不是 OpenAI 的服务端资源；本版没有 `/v1/files` HTTP 上传接口。CLI 注册不会立即上传到豆包，首次引用时上传。图片/PDF 已做真实模型读取；PNG/JPEG/WebP/GIF、PDF/TXT/MD/CSV/JSON/DOCX/XLSX/PPTX 已有类型映射，其他文档格式的解析效果尚未逐一验收。

工具结果也接受上述内容块数组。实测直接传 MCP 图片/PDF 不能保证 Work 理解，因此本版对含媒体的工具结果额外启动一个带真实附件的媒体读取任务，再把提取出的文本事实交回原 Work 任务。每个含媒体的工具结果会增加一次模型生成；读取任务也有独立收据，重启时复用，不重复生成。这个过程包含一次模型提取，不能保证无损保留所有图像细节；需要像素级后续操作的客户端不能把它当作原生多模态工具结果。普通 JSON 数组仍作为工具文本保留。

每个附件最多 8 MiB，每请求最多 12 个附件，整个请求最多 32 MiB。上传记录按账号与内容散列缓存 6 小时；不确定的上传日志不会自动重传，需要先核对。缓存和本地文件过期只删除本地记录，没有承诺删除豆包侧已上传的对象。

## 生命周期与恢复

插件的任务期限按每次等待计算，默认 10 分钟：豆包每次回答（给出下一个函数调用或最终回答）和客户端每次执行函数、交回结果，各自最多 10 分钟，整条工具链不限总时长。客户端过了期限才交回结果时，原任务已经结束，插件按完整对话记录开一个新任务继续，客户端不会收到错误。0.3.8 及更早的版本把整条工具链算作一个 10 分钟期限，Claude Code 的长任务会在中途返回 504 `task_expired`。

Work 自己只等每个客户端函数约 1 分钟。实测（2026-10-17，doubao-2.1-pro）：结果过 45 秒交回没有影响；过 120 秒或 170 秒交回时，豆包在上一次发出这个调用后 66–96 秒再发一次（参数相同，调用编号不同），这样重发一到两次后自己结束任务，回答里说调用失败、已重试。插件逐个处理调用，客户端交回前一个结果后，马上把重发的调用转给客户端，同一个函数因此执行了两到三次；客户端交回结果时任务已经结束的，插件按完整对话记录开新任务继续。所以客户端函数要在 1 分钟内交回结果，更慢的命令放到后台执行。

一个任务最多转交 32 次客户端函数调用（批处理模式下剩余次数不足 8 次就算用完）。用完以后，客户端下一次交回结果时，插件按完整对话记录开一个新任务继续，客户端不会收到错误；0.3.8 及更早的版本会返回 409 `upstream_failed`。

最多 4 个活动任务，最多保留 128 个任务。已结束任务的幂等窗口为 30 分钟。文件最多 128 个、总计 256 MiB，保留 7 天；上传缓存最多 256 项。过期本地数据在相应访问和扫描时清理。

CPA 正常退出/卸载时保留可恢复的任务绑定。重启后从已确认的原始 conversation/run 和工具日志恢复，call_id 不变，期限也不会重新计时。只有已确认接收的生成可以恢复；已发出但接收状态不明，或工具结果已写出但未确认，都返回 409 并保留日志，不会自动发起新生成或重传结果。重启前尚未提交的任务会明确结束并回收临时连接器。

客户端断开不等于取消原任务，以便用同一 key 重连。需要取消时：

```bash
/absolute/doubao-cpa/bin/doubao-helper -action task-list \
  -dir /absolute/doubao-cpa/state
/absolute/doubao-cpa/bin/doubao-helper -action task-cancel \
  -dir /absolute/doubao-cpa/state -task-id task-list返回的标识
```

取消会验证账户和精确原始 run，写入日志后只发送一次。取消请求受理与服务端实际停止分别核对。不确定的取消不重发。一个 state 目录只能由一个插件实例持有；helper 的文件操作另有进程间锁。

## 流式输出

无客户端工具的答案会实时发送已识别的可见正文块，过滤思考及其子块、未知隐藏容器，并用最终 IM 回读校验已发送前缀。若上游改写了已发出的正文，会以流错误结束，不能假装能撤回已发文本。客户端工具链在已验证的工具边界和最终回答处输出，暂不增量转发中间 Work 计划或工具参数。等待期间流式响应会定期发送 keepalive 注释。

## 验证与排障

```bash
go test -race ./...
```

真实 SDK 验收需要手工配置授权会话和独立目录，避免自动消耗账号：

```bash
export DOUBAO_TEST_DIR=/absolute/acceptance-dir
# build/ 中放 cli-proxy-api、doubao-work.so、doubao-helper；private/session.json 为已授权会话。
PYTHONDONTWRITEBYTECODE=1 python3 integration/native_probe.py
PYTHONDONTWRITEBYTECODE=1 python3 integration/acceptance.py
PYTHONDONTWRITEBYTECODE=1 python3 integration/extended_acceptance.py
```

SDK 仅为测试依赖。报告写进 `$DOUBAO_TEST_DIR/evidence/`，原始请求、账户会话、工具结果和上游日志在 `private/`。对外发送报告时只选 evidence 中已审查的文件；不要打包整个 state 目录。测试结果见 `docs/VALIDATION_CN.md`。
