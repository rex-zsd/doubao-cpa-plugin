# 0.3.21：更多原生工具转发

2026-10-22（UTC）已在线上官方 CPA v8.0.13 热加载 0.3.21。实现提交为 `407a178`。本批把 `native_tools: true` 的原生转发扩展到 NotebookEdit、Glob、Grep、TaskOutput、TaskStop 和后台 Bash。保持 Orange 5.5、原推理档位、完整历史和工具契约。

## 转发条件

只转发客户端明确声明的同名工具，同名还不够。客户端 schema 必须是 object，关键参数为字符串，required 字段只能是双方含义相同的参数。未经改写的调用还要通过客户端原始 JSON Schema 校验。

- NotebookEdit：绝对 notebook_path；cell_id、cell_type、edit_mode、new_source 按客户端 schema 原样转发。
- Glob：非空 pattern，path 省略或为绝对路径；只转发这两个参数。
- Grep：非空 pattern，path 省略或为绝对路径；glob、type、output_mode、-i/-n/-A/-B/-C、context、multiline、head_limit、offset 按客户端 schema 原样转发。
- 后台 Bash：客户端 Bash schema 声明了 run_in_background 才转发，不按工具名推断。
- TaskOutput/TaskStop：只用于客户端结果中返回的任务 ID，字段名按客户端 schema。

调用参数含 Work 独有的 Glob 排序、数量上限、目录/符号链接选项，或需要改写别名（Grep include、TaskOutput shell_id）时，继续走 MCP。参数或 Work 调用选项中的沙箱逃逸一律不转发。执行、审批和实际修改仍属于 HTTP 客户端。原生回复使用 content/isError 格式，并保留原始 native ID/input。

## 不支持的工具

客户端未声明或参数不兼容的工具，仍返回原来的 CLIENT_TOOL_ONLY 错误和 tool_search 路由提示，由 Work 改走 MCP。这里有意沿用原格式，没有改用 Work 官方客户端的未知工具回复，因此回复和恢复格式都不变。

## Work 侧的实测行为

- Glob 选项：实测的原生 Glob 调用都在调用选项里附带 glob_config，内容为按修改时间升序、最多 500 条、不含目录和符号链接、60 秒超时。探测中要求“最多 2 条、包含目录”，这些值仍然不变。可见它是平台固定默认值，不代表模型意图。插件不读取也不转发它，Glob 只转发 pattern/path，结果按客户端自己的 Glob 语义返回。
- NotebookEdit：真实目录的原生说明已经列出 NotebookEdit，但两个样本中 Work 都经 MCP 连接器调用它，没有出现原生 NotebookEdit 调用。原生转发由离线回归覆盖，线上原生路径尚未实测证明。MCP 路径的结果逐字正确。
- 客户端未声明 Glob：实测的 Claude Code 工具目录（32 个工具）没有 Glob、Grep、TaskOutput。问“目录下有几个 .go 文件”时，Work 直接改用原生 Bash 完成计数，没有产生被拒的 Glob 调用。这类客户端的新增收益主要是后台 Bash 和 TaskStop；Glob、Grep、TaskOutput 只对声明了它们的客户端生效。

## 离线回归

`gofmt -l .` 为空，`go vet ./...` 和 `go test -race -count=1 ./...` 全部通过。回归覆盖 Read、Write、Edit、Bash、NotebookEdit、Glob、Grep、TaskOutput、TaskStop 九种原生工具，检查以下各项：

- 参数逐字保留。
- HTTP 重放不重复执行。
- 真实结果和错误标志如实回传。
- 全局开关关闭后，等待中的调用仍可恢复：只上传一次，不另发生成。

不转发的用例包括：

- Glob 排序、数量上限和目录选项。
- 相对路径。
- Grep include 别名和 schema 之外的输出模式。
- TaskOutput 的 shell_id 别名、类型错误和超时上限。
- TaskStop 缺失、空或数字 ID。
- 相对 notebook 路径和 schema 之外的 notebook 模式。
- 后台运行参数的类型错误。

另外验证了以下几点：

- 客户端 Bash schema 没有 run_in_background 时，后台运行不转发。
- 同名工具的 required 契约不同则不转发。
- Work 调用选项中的沙箱逃逸不转发。
- 客户端未声明 Glob/Grep/TaskOutput 时，原生说明不列出它们，调用返回 CLIENT_TOOL_ONLY 和 tool_search 提示。

## 隔离实例实测

隔离实例使用独立设备标识和私有状态。测试客户端在临时目录里真实执行每个调用：启动后台进程、终止进程组、匹配文件和文本、修改 notebook。Bash 只允许在测试目录内执行只读 find/ls。

请求均为 Orange 5.5、effort 7，经 Chat Completions 非流式发送，关闭并行。“真实目录”是实测 Claude Code 的 32 个工具。“扩展目录”在此基础上加入 Claude Code 风格的 Glob、Grep、TaskOutput，共 35 个。

| 场景 | 目录 | Work 实际调用 | 客户端核对 | 整轮 |
|---|---|---|---|---:|
| Notebook 替换 → 插入 → 删除 | 真实 | 3 次 MCP NotebookEdit | 最终 notebook 逐字一致 | 36.478 / 34.123 秒 |
| 后台 Bash → TaskStop | 真实 | 原生 Bash（后台）、原生 TaskStop | 进程组已终止，无输出 | 22.126 秒 |
| 后台 Bash → TaskOutput | 扩展 | 原生 Bash（后台）、原生 TaskOutput（block） | 输出逐字一致 | 20.357 秒 |
| Glob 计数 | 扩展 | 原生 Glob（pattern、path） | 计数正确 | 15.089 秒 |
| Grep 定位 | 扩展 | 原生 Grep（pattern、path、output_mode、-n） | 匹配逐字一致 | 20.337 秒 |
| 无 Glob 时计数 | 真实 | 原生 Bash，无被拒调用 | 计数正确 | 15.979 秒 |
| Glob 排序/数量探测 | 扩展 | 原生 Glob，随后尝试 Bash | 测试端中止，见下文 | — |
| Read → Write → Edit → Read 回归 | 真实 | 4 次原生 | 文件逐字一致 | 36.966 秒 |

除探测外，每项都通过以下检查：

- 每个工具回合的相同 HTTP 请求重放不重复执行。
- 上游收到的结果和错误标志与客户端实际结果逐字一致。
- 完整目录、说明、schema 和历史保留。
- 只提交一次生成。

表中耗时都是单样本功能验收，不能用作性能对照。

Notebook 第一个样本的历史保留检查失败，属于测试脚本问题：它按 JSON 原文比较调用参数，把格式差异当成了内容变化。改为比较解析后的参数后重跑，第二个样本全部检查通过。第一个样本的其余检查也都通过。

Glob 探测中，Work 先原生调用 Glob（仅 pattern/path），再尝试用 Bash 自行排序截取。该命令超出测试端的只读范围，测试端按设计中止，没有回传结果。插件在单次等待超时后以 task_expired 结束该任务并完成清理，只提交了一次生成。这次中止由测试端发起，不是插件错误。

## 部署与复核

插件 SHA-256：`c69d50f99a5cf337689ee06456ddf5d7eb4c77b17cfb27108b5a5d3d3c41fc70`。

helper SHA-256：`184dab96d3d40940698accd2218668ee8ab3717a22fbd51136b3624b8c8afa2c`。

热更新在空闲窗口进行，前后都没有活跃任务或待清理连接器。容器 ID、模型列表和凭据启用状态保持。配置仅追加重载注释，语义与 inode 保持，`native_tools: true` 保留。已备份原配置、0.3.20 插件和 helper，可在任务清理后使用私有目录中的 rollback.py 热回滚。

上线后真实验收通过：后台 Bash → TaskStop 全程原生，用时 24.997 秒；Read → Write → Edit → Read 共 4 次原生调用，用时 36.208 秒。两项均逐字核对，完整 32 工具、Orange 5.5 和 effort 7 保留。

本批 9 个隔离任务和 2 个线上任务都已结束、清理，各自只提交一次生成；其中 1 个是上文中止的探测，以 task_expired 结束。独立设备测试 helper 已退出，测试端口关闭，所有本批租约均已释放。

脱敏证据见 [evidence-sanitized.json](evidence/native-more-tools-20261022/evidence-sanitized.json)，构建与离线回归记录在同目录。

部署、回滚、真实客户端及脱敏采集脚本保存在私有运维目录，不随源码发布。凭证、原始提示、工具输出和推理正文留在私有状态目录；交付证据仅含计数、名称、ID、耗时与校验结果。
