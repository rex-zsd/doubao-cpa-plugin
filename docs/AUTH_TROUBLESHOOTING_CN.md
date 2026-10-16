# 管理密钥与豆包登录

`missing management key` 是 CPA 管理接口返回的 401，表示该请求没有携带管理密钥。请求在进入豆包插件之前就已被拒绝，不能据此判断豆包 Cookie 是否有效。

## 三种凭据的用途

| 凭据 | 放在哪里 | 用途 |
|---|---|---|
| CPA 管理密钥 | `management.secret-key`；管理页面的管理密钥输入框 | 访问管理接口，包括插件管理和页面发起的登录操作 |
| CPA 模型 API key | `access.api-keys`；模型客户端的 API key 输入框 | 调用 `/v1/models`、Chat、Responses |
| 豆包登录会话 | 扫码后保存在插件配置 `state_dir` 下的 `session.json` | 插件访问豆包 |

当前插件提供扫码登录；尚未实现把原始 Cookie 文本粘贴到页面后导入的入口。不要直接用 Cookie 文本替换 `session.json`，它是插件自己的结构化会话文件。

## 使用管理页面

默认安装走 helper 扫码流程，管理页面关闭，也不生成管理密钥。如果要使用管理页面，把以下字段合并到**正在运行的 CPA 使用的配置文件**的现有 `management` 节点，不要新增第二个同名节点：

```yaml
management:
  allow-remote: false
  secret-key: "替换为你自己生成的管理密码"
  disable-control-panel: false
```

重启 CPA，在管理页面的登录/连接设置中填写这串管理密码。首次启动后 CPA 可能把配置中的密码改成 bcrypt 哈希；页面仍然填写原始密码。若原始密码已丢失，可重新设置上述字段并重启。

这里按本机访问说明；若 CPA 在另一台机器，可通过 SSH 本地端口转发访问。本段配置不会开启远程管理。

直接调用管理接口时，请在 HTTP 请求头中传 `X-Management-Key: <原始管理密码>` 或 `Authorization: Bearer <原始管理密码>`。豆包 Cookie 所在的 Cookie 请求头不能代替管理请求头。若页面已经登录但仍报 `missing management key`，在浏览器 Network 中检查失败请求是否实际携带了上述请求头，并检查中间代理是否保留请求头。

## 直接扫码登录

也可以继续使用安装包的 helper 流程，不经过管理页面。停止持有同一 state 目录的 CPA 实例后，执行下面的命令，路径替换为实际安装路径：

```bash
/absolute/doubao-cpa/bin/doubao-helper -action login \
  -dir /absolute/doubao-cpa/state \
  -out /absolute/doubao-cpa/state/login-evidence
```

打开 `state/login-qr.png`，用豆包客户端扫码确认；确认 `state/login-status.json` 的 `status` 为 `verified` 后，再按 README 启动 CPA。

## 已复现的鉴权结果

在交付的修补宿主上，以隔离配置调用 `/v8/management/plugins`，没有发送豆包上游请求：

| 请求携带的凭据 | 响应 |
|---|---|
| 不携带鉴权 | 401 `missing management key` |
| 只携带合成 Cookie | 401 `missing management key` |
| 携带模型 API key | 401 `invalid management key` |
| 正确的管理密钥，放在两种受支持的管理鉴权头中 | 均为 200 |

公开验证记录见 `docs/evidence/management-auth-proof.json`。
