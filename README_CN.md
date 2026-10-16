# 豆包 Work · CLIProxyAPI 原生插件

独立 Go 协议实现，通过 CPA 的 C ABI 加载 `.so`，不依赖或执行 doubao-cli，也不需要额外启动网关进程。当前交付针对 Linux amd64、单账号。

豆包 Work 实际运行的是 agent。这个插件把其对话和真实连接器调用转换成 LLM API；系统消息优先级、温度、token 预算等底层模型语义不能因此自动获得。

## 安装与启动

固定宿主版本为 `router-for-me/CLIProxyAPI@5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`。必须使用交付的修补宿主，或应用 `patches/cpa-http-wire.patch` 后自行构建。原版宿主缺少所需的重定向控制，不能直接替换使用。

从 [v0.1.0 Release](https://github.com/rex-zsd/doubao-cpa-plugin/releases/tag/v0.1.0) 下载插件包、修补宿主包及 `SHA256SUMS`。安装方法见 [二进制安装说明](docs/BINARY_INSTALL_CN.md)。当前为实验版 v0.1.0。

管理页面的管理密钥、模型 API key 和豆包登录会话用途不同。`missing management key` 的配置与排查见 `docs/AUTH_TROUBLESHOOTING_CN.md`。本版提供扫码登录，尚未提供原始 Cookie 文本的页面导入入口。

运行依赖 Linux amd64、glibc 2.34 或更新、`qrencode`；安装脚本还需要 Python 3。扫码辅助程序生成本地二维码，账户凭据只写在私有 state 目录。

解压二进制包后：

```bash
./scripts/install.sh /absolute/doubao-cpa
/absolute/doubao-cpa/bin/doubao-helper -action login \
  -dir /absolute/doubao-cpa/state -out /absolute/doubao-cpa/state/login-evidence
```

打开 `state/login-qr.png`，使用豆包客户端扫码并确认。看到 `state/login-status.json` 的 `status=verified` 后启动：

```bash
/absolute/doubao-cpa/bin/cli-proxy-api --local-model \
  --config /absolute/doubao-cpa/config.yaml
```

安装脚本生成独立 API key，保存于权限为 0600 的 `config.yaml`，默认监听 `127.0.0.1:18322`。客户端使用 `http://127.0.0.1:18322/v1`，model 选 `doubao-work-pro`；其他实时可用模型见 `/v1/models`。不要复用整个示例配置覆盖已有 CPA 配置，可把插件配置、账号标记和相关重试配置合入原有文件。

模型清单在首次打开服务时加载并缓存，重启后重新查询；没有固定 8 项的上限。GPT 等模型的数字 API ID 与显示名映射、清单排查见 `docs/MODEL_CATALOG_CN.md`。

若从源码构建，需要 Go 1.26、CGO、C 编译器，以及固定版本的 CPA checkout：

```bash
git clone https://github.com/rex-zsd/doubao-cpa-plugin.git
cd doubao-cpa-plugin
git clone https://github.com/router-for-me/CLIProxyAPI.git /absolute/CLIProxyAPI
git -C /absolute/CLIProxyAPI checkout 5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3
./scripts/build.sh /absolute/build-output /absolute/CLIProxyAPI
python3 scripts/package.py --build-dir /absolute/build-output --out-dir /absolute/packages
```

此命令运行 race 回归，构建插件和 helper，校验、应用宿主补丁并构建宿主。构建目录可自行指定；本仓库不保存任何登录会话。

## Chat、Responses 与工具

支持 `/v1/chat/completions` 和 `/v1/responses`，包含非流式和流式。每次都必须携带完整可见历史。`instructions` 和 system/developer 消息会保留为带角色的对话记录，不承诺原生 system 优先级。`previous_response_id`、后台 Responses 存储不支持。

客户端 functions 通过真实 MCP 连接器提供给 Work。收到标准 `tool_calls` / `function_call` 后，由客户端执行，再把原始 assistant 调用和 tool 结果一起回传；插件将结果交回**原来的 Work 任务**。本地 shell、读写文件等未声明工具不会被执行。

显式设置 `parallel_tool_calls=true` 时，原生端提供 `client_tools_batch`，模型在一次真实调用里选择 1–8 个独立函数。API 拆成多个 call_id，由客户端并行执行。一个批次的结果必须在同一请求内全部提交，顺序可以改变；依赖前一批结果的调用应进入下一批。当前没有证明 Work 原生可以同时挂起多个独立调用，批处理方式是本适配器明确提供的语义。

首次请求建议带 `Idempotency-Key`（最多 128 字符）。同一 key 和相同请求会复用原任务；相同 key 配不同请求返回 409。工具结果按 call_id 去重，冲突结果返回 409。请配置 SDK `max_retries=0`，CPA `request-retry=0`、`bootstrap-retries=0`、`max-retry-credentials=1`，避免其他层将不确定写请求作为新请求重试。

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

默认任务期限 10 分钟，包含等待客户端工具结果的时间；最多 4 个活动任务，最多保留 128 个任务。已结束任务的幂等窗口为 30 分钟。文件最多 128 个、总计 256 MiB，保留 7 天；上传缓存最多 256 项。过期本地数据在相应访问和扫描时清理。

CPA 正常退出/卸载时保留可恢复的任务绑定。重启后从已确认的原始 conversation/run 和工具日志恢复，call_id 不变，期限也不会重新计时。只有已确认接收的生成可以恢复；已发出但接收状态不明，或工具结果已写出但未确认，都返回 409 并保留日志，不会自动发起新生成或重传结果。重启前尚未提交的任务会明确结束并回收临时连接器。

客户端断开不等于取消原任务，以便用同一 key 重连。需要取消时：

```bash
/absolute/doubao-cpa/bin/doubao-helper -action task-list \
  -dir /absolute/doubao-cpa/state
/absolute/doubao-cpa/bin/doubao-helper -action task-cancel \
  -dir /absolute/doubao-cpa/state -task-id task-list返回的标识
```

取消会验证账户和精确原始 run，写入日志后只发送一次。取消请求受理与服务端实际停止分别核对。不确定的取消不重发。一个 state 目录只能由一个插件实例持有；helper 的文件操作另有进程间锁。

## 流式、usage 和兼容边界

无客户端工具的答案会实时发送已识别的可见正文块，过滤思考及其子块、未知隐藏容器，并用最终 IM 回读校验已发送前缀。若上游改写了已发出的正文，会以流错误结束，不能假装能撤回已发文本。客户端工具链在已验证的工具边界和最终回答处输出，暂不增量转发中间 Work 计划或工具参数。

上游没有提供可验证 token 用量。Chat 不返回 usage；固定 CPA 版本转换出的 Responses 可能补零，这些零是兼容占位，不是测量值。响应附 `X-Doubao-Usage: unavailable`，不能据此计费。`stream_options.include_usage=true` 和精确 token 计数会明确拒绝。

只接受 `tool_choice=auto/none`、`n=1`。温度、top_p、max_tokens/max_output_tokens、stop、严格 schema、强制工具选择、reasoning 控制、音视频、原生多模态输出等不在本版支持范围。不承诺完整 Codex/Claude Code 客户端兼容性。

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

SDK 仅为测试依赖。报告写进 `$DOUBAO_TEST_DIR/evidence/`，原始请求、账户会话、工具结果和上游日志在 `private/`。对外发送报告时只选 evidence 中已审查的文件；不要打包整个 state 目录。401 通常需要重新扫码，409 先核对本地任务/上传日志，429 先查看容量与待结束任务。安装包、配置和宿主补丁的测试结果见 `docs/VALIDATION_CN.md`。
