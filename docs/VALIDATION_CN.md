# 验收记录

## 2026-10-16 · 0.3.7 · 官方 CPA v8.0.13

插件运行在未修补的官方 CPA v8.0.13 里，由插件自带的 HTTP 客户端访问豆包。平台为 Linux amd64，单账号。`.so` 和 helper 由 `scripts/build.sh` 构建，按带版本号的文件名热加载。耗时都在客户端测量，包含模型思考的时间。测试期间，同一账号上还有其它客户端在用。

`go test -race -json ./...`：70 项通过（含子用例，顶层 67 项），0 失败，1 项真实账号诊断默认跳过；`gofmt` 和 `go vet` 无输出。

### 接口与参数

| 场景 | 模型 | 请求 | 结果 |
|---|---|---|---|
| 模型列表 | — | `/v1/models` | 9 个：`doubao-auto`、`doubao-2.1-lite`、`doubao-2.1-pro`、`doubao-2.1-turbo`、`doubao-orange-5.5`、`doubao-gpt-6.1-sol`、`doubao-gpt-5.6-sol`、`doubao-gpt-6-astra`、`doubao-gemini-3.7-flash`。旧名字 `doubao-work-<编号>` 立即返回 400 `model_not_found` |
| Chat | doubao-2.1-lite | system 消息，加上 `temperature`、`top_p`、`max_tokens`、`stop`、`presence_penalty`、`frequency_penalty`、`seed`、`user`、`n=1` | 200，6.0 s。采样参数被忽略，usage 为估算值 |
| Chat 流式 | doubao-2.1-lite | `stream`、`stream_options.include_usage` | 200，4.4 s，13 帧 |
| 结构化输出 | doubao-2.1-lite | `response_format` 为 `json_schema`，`strict: true` | 200，7.6 s，输出符合结构、可直接解析的 JSON |
| Claude 函数调用 | doubao-2.1-pro | `/v1/messages`，1 个函数 | 9.5 s 返回 `tool_use`；交回结果后流式回答 4.4 s，`end_turn` |
| Codex 函数调用 | doubao-gemini-3.7-flash | `/v1/responses` 流式，`reasoning.effort=xhigh`，shell 函数 | 23.5 s 返回 `function_call`；交回结果后 6.7 s 回答 |
| Responses 其它参数 | doubao-2.1-lite | `max_output_tokens`、`temperature`、`top_p`、`truncation`、`metadata`、`service_tier`、`prompt_cache_key`、`include`、`text.verbosity`、`store: false`，`tool_choice` 指定函数 | 200，6.1 s，返回指定函数的调用 |
| 命名空间函数 | doubao-2.1-pro | `additional_tools` 里的命名空间函数，`tool_choice` 指定 `clock` 命名空间的 `sleep` | 200，14.9 s，返回带 `namespace: clock` 的 `function_call` |
| custom 工具 | doubao-2.1-pro | `additional_tools` 里的 custom `exec`（lark 语法），说明约 27,000 个字符 | 18.1 s 返回 `custom_tool_call`；交回结果后 4.3 s 答出 42 |
| 超长函数说明 | doubao-2.1-pro | 同上，说明约 37,800 个字符，超过 Work 的上限，完整说明放进提示词 | 24.8 s 返回 `custom_tool_call`；交回结果后 4.9 s 答出 42 |
| ModelTrace candy-eval 同类请求 | doubao-gpt-6.1-sol | 按 candy-eval 格式的流式请求，`additional_tools` 含 functions、clock、collaboration 三个命名空间 | 200，39.5 s，按要求输出 300 个整数 |
| Gemini 流式 | doubao-2.1-lite | `streamGenerateContent`，`systemInstruction` | 200，4.2 s，11 帧 |
| Gemini 函数调用 | doubao-2.1-pro | 参数类型写成大写的 `OBJECT`、`STRING` | 12.6 s 返回 `functionCall`；交回结果后流式回答 7.9 s |
| 并行调用 | doubao-2.1-pro | 3 个函数，`parallel_tool_calls=true` | 16.2 s 一次返回 3 个调用；交回结果后 13.1 s 回答 |
| 批处理回退 | doubao-2.1-pro | 13 个函数，`parallel_tool_calls=true`，批处理结构超过 48,000 个字符 | 3 次都完成：第一个调用 15.6–32.3 s，后两个各 0.8–1.4 s，回答 5.3–34.4 s |
| 超大参数结构 | doubao-2.1-pro | 一个函数的参数结构 68,600 个字符，订单号 ZX-4471 只写在结构里 | 15.2 s 返回调用并填对 ZX-4471；交回结果后 4.3 s 答出回执号 R-889 |
| 图片输入 | doubao-2.1-pro | Chat `image_url`（PNG data URL）；Claude base64 图片块 | 随机生成的红、蓝方块数都答对：Chat 46.2 s，Claude 14.8 s |
| PDF 输入 | doubao-2.1-pro | Responses `input_file`（PDF data URL） | 19.6 s，随机校验码和两个金额之和都答对 |
| 超长请求 | doubao-2.1-lite | 300,000 个字符的 Claude（流式和非流式）、Chat、Codex 请求 | 0.1–0.2 s 返回 400：Claude 为 `invalid_request_error`，Chat 和 Codex 为 `context_length_exceeded`，消息都是 `prompt is too long: 300000 tokens > 262144 maximum …` |
| Claude Code | doubao-gpt-6.1-sol | `claude -p` 读一个文件，再用 Bash 运行 `wc -c`。Claude Code 提供 20 个工具，haiku 模型设为 doubao-2.1-lite | 4 次都答对：46.3、50.4、69.8、50.4 s |

第 3 次 Claude Code 用了 69.8 s。当时同一账号的另一个客户端同时开了 3 个任务，插件按规则挤掉了正在等函数结果的任务，其中包括这次 Claude Code 的任务。Claude Code 交回结果时，插件要用完整历史开新任务，但 4 个位置都在运行，先返回了 4 次 429 `gateway_capacity`。Claude Code 自动重试，十几秒后新任务开始，最后仍然答对，共发了 7 个请求（正常是 3 个）。另一个客户端的任务都正常完成。

### 并发

三个请求间隔 2 s 发出，同时进行，都用 doubao-2.1-pro。“首轮”是返回函数调用的耗时，“回答”是交回结果后的耗时：

| 请求 | 0.3.4 首轮 | 0.3.5 首轮 | 0.3.5 回答 | 0.3.7 首轮 | 0.3.7 回答 |
|---|---|---|---|---|---|
| 客户端自己的 `run_batch` 函数，数组项为 `oneOf` | 283.2 s | 14.1 s | 7.4 s | 10.3 s | 5.1 s |
| 同上，数组项为 `anyOf` | 167.0 s | 29.6 s | 10.3 s | 14.9 s | 5.4 s |
| 3 个函数，`parallel_tool_calls=true` | 14.8 s | 13.2 s | 5.6 s | 16.1 s | 5.4 s |

0.3.4 那一轮同时有 4 个请求，第 4 个（数组项为普通对象的 `run_batch`）首轮 124.8 s。Work 把连接器调用都从最新打开的工具通道发下来；0.3.4 会丢掉不属于该通道任务的调用，较早的任务要等较新的任务结束才拿得到调用。0.3.5 起把每个调用转交给它所属的任务。

### 连接器函数的可见性

Work 只通过自带的 `tool_search` 把连接器工具交给模型，每次返回 3 个工具。连接器工具在 Work 里的名字是 `mcp__` 加连接器名（`-` 换成 `_`）、`__` 和函数名（转成小写，`-` 换成 `_`）。不同搜索词的实测结果：

| 搜索词 | 结果 |
|---|---|
| 完整工具名 | 连接器工具排第一 |
| 只写函数名 `Read` 或 `Bash` | 排第二，在同名的 Work 自带工具之后（各 2/2） |
| 一次搜几个函数（`Read Bash`） | 3 个结果都是 Work 自带工具 |
| 写法与 Work 的名字不同（`read-File`） | 同上 |
| 描述（“读取文件内容”） | 同上 |

搜不到时，模型不调用客户端函数，直接回答。不先搜索、直接用 `execute_defer_tool` 加完整名字也能调用成功（doubao-2.1-pro，23.4 s）；doubao-gpt-6.1-sol 不按诊断要求列出自己的工具，也不听“不要搜索”的要求。13–20 个函数的测试中，模型调用了客户端函数的次数：

| 测试 | 0.3.5 及更早 | 0.3.6 | 0.3.7 |
|---|---|---|---|
| Claude Code（doubao-gpt-6.1-sol，20 个工具） | — | 1/3 | 4/4 |
| 批处理回退（doubao-2.1-pro，13 个函数） | — | 3/3 | 3/3 |
| 直接发 Claude Code 的 20 个工具定义（sol、pro 各 1 次） | — | 2/2 | 2/2 |
| 合计 | 7 次里 2 次没调用（回退 1 次、Claude Code 1 次） | 6/8 | 9/9 |

0.3.5 及更早的提示词只给出连接器名；0.3.6 加上函数名；0.3.7 列出每个函数的完整工具名，要求每次只按一个完整名字搜索。

### Work 实测限制

- 一条消息最多 262,144 个字符，`/v1/models` 据此报 65,536 token 的上下文。
- 单个连接器工具的说明 33,515 个字符可用，39,995 个字符时整个工具被静默丢弃；说明加参数结构合计约 66,000 个字符以内能可靠读到。

### 本轮修复的问题

- 模型名：按 Work 的显示名生成 `doubao-2.1-pro` 这类名字，替代 `doubao-work-<编号>`。旧名字由 CPA 返回 400 `model_not_found`，直接发到插件的未知模型返回 400 `invalid_model`。
- 请求解析：采样参数接受后忽略；不再拒绝 `stream_options.include_usage`、`strict` 函数、`store`、`reasoning`、`max_output_tokens`、推理回放条目和强制 `tool_choice`；去掉 256 条消息和 20,000 字节说明的上限。0.3.4 起支持 Codex Responses Lite 的 `additional_tools` 和命名空间函数，ModelTrace 报的 `unsupported_input: Unsupported Responses input item type: additional_tools` 由此解决；同时支持 custom 工具。Gemini 的大写类型名转成小写。`reasoning_effort` 和 `reasoning.effort` 映射到 Work 的思考档位。`response_format` 和强制 `tool_choice` 变成提示词要求。
- 长度：超过 Work 读取上限的说明和参数结构放进提示词；批处理结构超过 48,000 个字符时改为逐个提供函数；超长请求返回 Claude Code 和 Codex 会据此压缩上下文的 `prompt is too long` 措辞。
- 并发：0.3.5 起把每个连接器调用转交给它所属的任务。任务满 4 个时，先挤掉最接近期限、正在等函数结果的任务，没有这样的任务才返回 429 `gateway_capacity`。
- 连接器函数的可见性：见上节，0.3.6 和 0.3.7 两次修改提示词。
- 错误：豆包在确认收到前拒绝的请求（`STREAM_ERROR`）返回 422 `upstream_rejected`，附带豆包的错误码。
- 流式：CPA 为 Claude、Gemini 和 Responses 客户端转换流时只读 `data:` 行，Claude 的转换器会丢掉其它内容，插件现在对这些流输出 `data:` 行。响应头 `X-Doubao-Usage: estimated` 标明 usage 是估算值。HTTP/2 连接 30 s 没有收到数据就发 ping，15 s 没有回应即断开。这只是兜底：测试中的多分钟卡顿查明是并发问题，没有观察到连接静默断开。
- 登录：helper 扫码换了会话后，插件空闲时重新打开会话，不用重启 CPA；二维码在进程内生成，不依赖 `qrencode`；helper 静态编译，容器里和宿主机上都能运行。

### 当前边界

usage 是估算值，不能计费。每个请求都是一个 Work 任务，会留在账号的 Work 历史里，插件不删除。热加载不等进行中的请求结束，升级要选空闲的时候。函数调用和结构化输出靠提示词，不是强制约束。超过 4 个任务时，不重试 429 的客户端会直接收到容量错误。

本轮没有在官方宿主上重测本地 file_id、HTTPS file_url、工具媒体结果、主动取消、重启恢复和 `Idempotency-Key`，这些仍以 0.1.0 在修补宿主上的验收为准。本轮沿用已有授权账号，没有重新扫码。


## 2026-10-15 · 0.1.0 · 修补宿主

实现以独立 Go 核心和 CPA 原生 C ABI 插件运行。所有真实客户端请求均通过 CPA 进程中的插件，再通过 CPA 的 host HTTP 桥接访问豆包；未导入或执行 doubao-cli。固定宿主 commit 为 `5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`，包含本仓库提供的 HTTP 桥接补丁。

### 已验证

| 范围 | 实际验证 |
|---|---|
| 动态加载/账号/模型 | 修补宿主加载 `.so`，读取现有已授权会话，实时注册模型；官方 OpenAI SDK 文本随机校验串精确匹配 |
| Chat / Responses 增量 | 两种 API 都在回答结束前收到多个正文 delta；Responses 最终文本与 delta 拼接一致；可见块 reducer 的独立实测产生 645 次更新，最终回读前缀一致 |
| 图片与 PDF 输入 | Chat 与 Responses 实际读取合成 PNG/PDF，正确识别 3 个红方形、3 个蓝圆形、文件内随机码与两金额之和 11137 |
| 本地 file_id | 注册 PDF、Chat 引用、真实读取、删除；隔离/过期/校验和/路径拒绝由回归覆盖 |
| HTTPS file_url | Responses 经带代理的宿主下载合成随机内容，模型精确返回随机校验码；URL 在发送给模型前被附件标记替换 |
| 多工具 + 重启 | Responses 一次暴露 3 个模型选择的调用；客户端实际重叠执行、按不同顺序回传；CPA 停止/重启后 call_id 不变，原任务续跑，结果重放一致 |
| 工具媒体结果 | Chat 和 Responses 的工具结果携带真实 PNG/PDF，适配器媒体读取后，原任务正确回答图形数、文件码和金额；每个含媒体结果额外一次读取生成 |
| 主动取消 | 对本次创建的精确原 run 发送一次取消，回读确认服务端停止；迟到的工具结果返回 409，主生成次数仍为 1 |
| 状态/错误恢复 | 完成缓存重启重放、未完成批次恢复、不确定上传拒绝重发、已过期任务不重新计时、取消不重发、工具历史/结果冲突、持久化失败回滚均由回归覆盖 |

`go test -race -json ./...`：45 项通过（含子用例），0 失败，1 项真实账号诊断默认跳过。HTTP 宿主针对重定向控制、公网地址筛选、代理下固定目标 IP 与 TLS 身份保留的 3 项回归通过；宿主服务端构建通过。

二进制包已解压并通过随包脚本安装到独立目录；安装后的宿主、插件和生成配置实际启动成功，模型注册正常。已核对二进制与验收版本一致、私有文件权限和压缩包校验和，并逐文件确认未包含现有登录凭据。

公开证据为 `acceptance-summary.json`、`native-smoke.json`、`cancellation.json`、`unit-summary.json`、`host-test.log`、`package-validation.json`。完整请求和上游收据仅保存在任务私有目录，未进入源码或交付包。审计计数只证明请求次数、状态和连接器清理，不代替 SDK 对答案正确性的断言。

### 验收过程中修复的问题

- 原生 HTTP 桥接没有透传请求写出信号，曾使工具通道已打开却不启动生成；现以收到上游响应头作为写出确认。
- Chat handler 会添加 SSE envelope，插件曾重复封装；现在对宿主只输出 JSON chunk，结束事件由宿主产生。
- 持久化对象的浮点数解析曾导致过期时间为零、缓存失效；统一兼容 json.Number 和 JSON 数字，加入文件/恢复回归。
- CPA 默认 Responses 输入转换会遗漏 file_url；插件直接接收并规范化 Responses 全历史，保留媒体源。
- 单次 DNS 预检查不够；宿主现在固定已验证 IP，经过代理仍按 IP CONNECT，保留原 HTTP Host 和 TLS SNI。
- 直接传 MCP 图片/PDF 后，Work 曾再次请求同一工具；这没有被认定为功能通过。最终改用有独立收据的媒体读取任务，两个 API 的完整答案断言均通过。

### 当前边界

只验收单账号、Linux amd64 和全历史 Chat/Responses。图片/PDF 已真实读取，其余有映射的文件格式没有逐项验证。工具媒体结果会增加模型调用，且提取为文字，不能承诺无损视觉细节。客户端工具链的中间 Work 计划和参数不做增量透传。

没有 /v1/files HTTP 上传；使用 inline、HTTPS URL 或 helper 的 file_id。Responses 的零 usage 是宿主兼容占位，不能计费。系统角色是显式对话记录，强制工具、温度、token 预算、严格结构化输出和完整 Codex/Claude Code 兼容不在验收范围。

本轮沿用已有授权账号，没有要求用户重复扫码。扫码核心此前已验证，本轮修订过的插件登录流程生命周期未重新完成一次人工扫码验收。正常停止/重启已做真实测试；不确定写与重启过期路径通过确定性回归覆盖，没有故意制造服务端不可核对的网络故障。
