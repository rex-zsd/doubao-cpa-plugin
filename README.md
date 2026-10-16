# Doubao Work native plugin for CLIProxyAPI

[中文文档](README_CN.md) · [Binary installation](docs/BINARY_INSTALL_CN.md) · [Validation](docs/VALIDATION_CN.md)

An independent Go protocol adapter exposed through CLIProxyAPI's native C ABI. No doubao-cli runtime dependency and no extra HTTP gateway process. This experimental v0.1.0 targets a single Doubao Work account on Linux amd64.

Download **both** `doubao-cpa-plugin-0.1.0-linux-amd64.tar.gz` and `cpa-host-doubao-0.1.0-linux-amd64.tar.gz`, plus `SHA256SUMS`, from the repository's **Releases** page. Verify the two archives, extract them into the same directory, and follow [the installation guide](docs/BINARY_INSTALL_CN.md) for QR login and startup. Packages contain no account credentials. The default API is `http://127.0.0.1:18322/v1`; the installer creates a private API key in its `config.yaml`.

The host is pinned to [CLIProxyAPI commit 5fc580b](https://github.com/router-for-me/CLIProxyAPI/tree/5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3) and requires [the supplied HTTP bridge patch](patches/cpa-http-wire.patch). Use the bundled patched host or rebuild the pinned source. The patch controls redirects and pins media downloads to validated public IPs, including through proxies.

Build with Go 1.26, CGO and a C compiler:

```bash
git clone https://github.com/router-for-me/CLIProxyAPI.git /absolute/CLIProxyAPI
git -C /absolute/CLIProxyAPI checkout 5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3
./scripts/build.sh /absolute/output /absolute/CLIProxyAPI
python3 scripts/package.py --build-dir /absolute/output --out-dir /absolute/packages
```

Supported: full-history Chat Completions and Responses, visible text streaming, image/file inputs, client function tools, model-selected batches of up to eight functions, persistent pending calls and restart recovery. See [README_CN.md](README_CN.md) for protocol details and tested limits.

This wraps a Work agent. System/developer messages are an explicit transcript, without native system-priority guarantees. Token usage is unavailable; temperature, token budgets, strict schemas and forced tool selection are unsupported. There is no `/v1/files` endpoint; use inline data, HTTPS URLs or helper-managed file IDs. Each media-bearing tool result requires one additional model generation to extract textual observations. Cookie-paste import is not implemented; use QR login. Full Codex/Claude Code compatibility has not been established.

Account sessions, request receipts and tool results stay in the configured private state directory. Integration tests require an explicitly configured authorized local session; ordinary unit tests do not use an account. Licensing and source acknowledgments are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
