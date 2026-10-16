# 二进制安装包使用

当前为实验版 v0.1.0，只验收 Linux amd64、单账号。二进制在 Debian 12 / glibc 2.36 上构建，要求 glibc 2.34 或更新。其他环境请从源码构建。扫码登录需要安装 `qrencode`，安装脚本需要 Python 3。

## 从 GitHub Release 安装

从本仓库的 **Releases** 页面下载以下文件，放到同一个空目录：

- `doubao-cpa-plugin-0.1.0-linux-amd64.tar.gz`：插件、helper、安装脚本和文档。
- `cpa-host-doubao-0.1.0-linux-amd64.tar.gz`：修补后的 CPA 宿主。
- `SHA256SUMS`：发布包的 SHA-256 校验值。

只校验已下载的两个二进制压缩包，再依次解压和安装：

```bash
awk '$2 == "doubao-cpa-plugin-0.1.0-linux-amd64.tar.gz" || $2 == "cpa-host-doubao-0.1.0-linux-amd64.tar.gz"' SHA256SUMS > BINARY_SHA256SUMS
sha256sum -c BINARY_SHA256SUMS
tar -xzf doubao-cpa-plugin-0.1.0-linux-amd64.tar.gz
tar -xzf cpa-host-doubao-0.1.0-linux-amd64.tar.gz
cd doubao-cpa-plugin-0.1.0-linux-amd64
./scripts/install.sh /absolute/doubao-cpa
```

两个二进制包解压到同一目录，分别提供插件/helper 和修补宿主。安装脚本用于新目录；升级已有部署时先停服务并备份自己的配置与私有 state，避免覆盖账号资料。`doubao-cpa-plugin-0.1.0-source.tar.gz` 是可选源码包，不需要解压到二进制目录。GitHub 自动生成的 Source code 也不包含编译好的插件与宿主。

## 扫码和启动

```bash
/absolute/doubao-cpa/bin/doubao-helper -action login \
  -dir /absolute/doubao-cpa/state \
  -out /absolute/doubao-cpa/state/login-evidence
```

打开安装位置的 `state/login-qr.png`，用豆包客户端扫码确认。`state/login-status.json` 的 `status` 为 `verified` 后启动：

```bash
/absolute/doubao-cpa/bin/cli-proxy-api --local-model \
  --config /absolute/doubao-cpa/config.yaml
```

默认 API 为 `http://127.0.0.1:18322/v1`，API key 在安装位置权限为 0600 的 `config.yaml`。便捷模型 ID 为 `doubao-work-pro`；完整可用清单查询 `/v1/models`。交付包不含账号凭据。

默认配置关闭管理页面。若遇到 `missing management key`，请按 [管理鉴权说明](AUTH_TROUBLESHOOTING_CN.md) 配置独立的管理密钥；模型 API key 和豆包 Cookie 都不能代替它。本版豆包登录入口为扫码，尚未实现原始 Cookie 文本的页面导入。

宿主固定到 `router-for-me/CLIProxyAPI@5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`。自行构建请按 [源码说明](../README_CN.md) 应用 `patches/cpa-http-wire.patch`；未修补的宿主不能直接使用此插件。工具媒体结果每项多一次模型读取，并以文本事实交回原任务，边界见 README 和 [验收记录](VALIDATION_CN.md)。

## 旧飞书分卷附件

GitHub Release 提供完整宿主包，不需要分卷。如果使用此前飞书发出的分卷附件，把两个分卷放在同一个目录后合并，再使用**同次交付的**校验文件：

```bash
cat cpa-host-doubao-0.1.0-linux-amd64.tar.gz.part01 \
    cpa-host-doubao-0.1.0-linux-amd64.tar.gz.part02 \
    > cpa-host-doubao-0.1.0-linux-amd64.tar.gz
```

旧附件与 GitHub Release 的文档版本可能不同，请勿混用各次交付的校验和。
