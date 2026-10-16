# 模型数量与名称

模型数量不固定。插件在首次打开服务时读取当前账号的 Work 模型清单，只注册可用且支持 Work 模式的项，每项一个 API ID。

## 名称规则

API ID 由 Work 显示名转换而来：去掉“豆包”，转小写，字母、数字和点以外的字符换成 `-`，再加上 `doubao-` 前缀。“自动”对应 `doubao-auto`。两个显示名转换后相同时，后注册的一个追加上游模型 key。

| Work 显示名 | API ID |
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

这是测试账号 2026-10-16 的清单，会随账号权限和上游配置变化，以 `/v1/models` 为准。“自动”不代表某个固定的底层模型。

0.3 之前的 API ID 是 `doubao-work-` 加上游模型 key（例如 `doubao-work-5262373378`），另有 `doubao-work-pro` 别名。这些名称已不再注册，用它们请求会返回 `model_not_found`，请改用上表的名称。

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
