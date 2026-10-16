# 模型数量与名称

模型数量没有固定为 8。插件在首次打开服务时读取当前账号的豆包模型清单，仅注册可用且支持 Work 模式的项，再为豆包 Pro 增加 `doubao-work-pro` 便捷别名。同一个底层模型可以有两个 API ID，所以 API ID 数量不等于底层模型数量。

本轮用现有授权账号和已交付的宿主/插件重新查询，豆包返回 9 个 Work 选项：8 个具体模型和“自动”。原生 CPA `/v1/models` 实际返回 10 个 ID，包括上述 9 项和 Pro 别名。没有提交模型生成请求。

## 当前账号的名称对应

| Work 显示名 | 插件 API ID |
|---|---|
| GPT-6.1 Sol | `doubao-work-5262373378` |
| GPT-6 Astra | `doubao-work-1964544258` |
| GPT-5.6 Sol | `doubao-work-2123520258` |
| Gemini 3.7 Flash | `doubao-work-1946880770` |
| Orange 5.5 | `doubao-work-5255447810` |
| 豆包 2.1 Turbo | `doubao-work-4` |
| 豆包 2.1 Pro | `doubao-work-5`，另有 `doubao-work-pro` |
| 豆包 2.1 Lite | `doubao-work-seed-lite-7b` |
| 自动 | `doubao-work-9` |

以上是当前授权账号在 2026-10-15 的真实清单，可能随账号权限和上游配置改变，不能作为所有账号的固定映射。“自动”也不代表一个指定的底层模型。

插件目前用 `doubao-work-` 加上游模型 key 作为 API ID，并另外提供显示名；固定 CPA 的 OpenAI `/v1/models` handler 只保留标准字段，丢弃显示名，因此仅查看该接口或客户端下拉框时，GPT 可能显示为数字 ID。上述 GPT-6.1 ID 证明该选项已注册到 API；本轮没有额外验证它的真实生成。

## 数量与页面不一致时

当前服务缓存首次加载的清单，没有定时刷新功能。若 Work 刚新增模型，可正常停止并重启 CPA，然后重新拉取客户端模型列表。读取的是配置中 `state_dir` 对应的账号；浏览器 Work 登录的账号若不同，可用模型也可能不同。

先比较**直接请求同一个 CPA 地址的 `/v1/models`**和客户端列表，确认服务地址、账号和列表缓存一致。模型列表接口使用 `access.api-keys` 中的模型 API key。管理页面使用独立管理密钥，区别见 `AUTH_TROUBLESHOOTING_CN.md`。

如要查看当前账号的上游显示名，可在停止持有同一 state 目录的 CPA 后，运行以下只读清单查询，并查看输出目录的 `models.json`；该操作不提交模型生成请求：

```bash
/absolute/doubao-cpa/bin/doubao-helper -action models \
  -dir /absolute/doubao-cpa/state \
  -out /absolute/doubao-cpa/state/model-evidence
```

原生 API 清单验证记录见 `docs/evidence/native-model-catalog.json`。
