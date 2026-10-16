#!/usr/bin/env bash
set -euo pipefail
bundle_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
prefix=${1:?Usage: install.sh ABS_INSTALL_PREFIX}
case "$prefix" in /*) ;; *) echo 'Install prefix must be absolute' >&2; exit 2;; esac
if [[ -e "$prefix/config.yaml" || -e "$prefix/plugins/doubao-work.so" ]]; then
 echo 'Existing installation found. Stop CPA and use a new prefix or perform a deliberate upgrade.' >&2;exit 2
fi
mkdir -p -- "$prefix/bin" "$prefix/plugins" "$prefix/auths" "$prefix/state"
chmod 700 -- "$prefix/auths" "$prefix/state"
install -m 755 "$bundle_dir/bin/doubao-helper" "$prefix/bin/doubao-helper"
install -m 755 "$bundle_dir/plugins/doubao-work.so" "$prefix/plugins/doubao-work.so"
if [[ -f "$bundle_dir/bin/cli-proxy-api" ]]; then install -m 755 "$bundle_dir/bin/cli-proxy-api" "$prefix/bin/cli-proxy-api"; fi
python3 - "$prefix" <<'PY'
import json,pathlib,secrets,sys
prefix=pathlib.Path(sys.argv[1])
# JSON-quoted absolute paths are valid YAML scalar strings, including spaces.
quote=lambda value:json.dumps(str(value),ensure_ascii=False)
config=f'''config-version: 8
server:
  host: 127.0.0.1
  port: 18322
management:
  disable-control-panel: true
access:
  api-keys: [{quote(secrets.token_urlsafe(32))}]
oauth:
  auth-dir: {quote(prefix/'auths')}
routing:
  strategy: fill-first
  retry:
    request-retry: 0
    max-retry-credentials: 1
requests:
  streaming:
    bootstrap-retries: 0
plugins:
  enabled: true
  dir: {quote(prefix/'plugins')}
  configs:
    doubao-work:
      enabled: true
      priority: 1
      state_dir: {quote(prefix/'state')}
      helper_path: {quote(prefix/'bin/doubao-helper')}
'''
(prefix/'config.yaml').write_text(config)
(prefix/'config.yaml').chmod(0o600)
(prefix/'auths/doubao-work.json').write_text('{"type":"doubao-work","version":1}\n')
(prefix/'auths/doubao-work.json').chmod(0o600)
PY
printf 'Installed to %s. Follow README_CN.md for login and startup.\n' "$prefix"
