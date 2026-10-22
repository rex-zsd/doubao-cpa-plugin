# 0.3.13：使用 CPA 已解析的模型名

CPA 的 OAuth 模型别名可以为豆包 Work 提供客户端兼容名称，例如：

```yaml
oauth-model-alias:
  doubao-work:
    - name: doubao-orange-5.5
      alias: claude-opus-5-5
      fork: true
      force-mapping: true
```

实际执行仍为豆包 Work 的 Orange 5.5。fork 保留原始 doubao-orange-5.5 名称，force-mapping 把返回的 model 字段改回客户端请求的别名。Claude 的原始凭证可独立停用，不影响这条豆包路由。

0.3.12 在不需要请求格式转换的路径上直接使用请求正文里的 model。CPA 已经把别名解析到执行请求的 Model 字段，但正文可能还保留客户端别名，因此同一别名在 Claude Messages 格式转换后可用，直接调用 OpenAI Chat Completions 却返回 invalid_model。

0.3.13 在 Responses 规范化之后，将正文 model 设置为宿主机解析后的 Model 字段，统一用于流式和非流式执行。使用 json.RawMessage 保留其余字段和大整数；未设置 Model 或原本已一致时沿用原正文。非法 JSON 留给原 API 校验器返回原有 400 错误。此修改不更改模型能力、工具定义、上下文或推理配置。

回归覆盖原生 Chat Completions 别名、工具 schema 中超出浮点精确范围的大整数、完整历史和最高思考档位保留，以及 Responses 格式规范化后的别名。go test -race ./...、go vet ./... 和 Linux amd64 C ABI 构建均通过。
