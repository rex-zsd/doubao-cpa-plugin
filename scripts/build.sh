#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${1:?Usage: build.sh ABS_OUTPUT_DIR [CPA_CHECKOUT]}
case "$output_dir" in /*) ;; *) echo 'Output directory must be absolute' >&2; exit 2;; esac
mkdir -p -- "$output_dir"
cd -- "$project_root"
export CGO_ENABLED=1
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.0}
go test -race ./...
go build -trimpath -buildvcs=false -buildmode=c-shared -o "$output_dir/doubao-work.so" ./cmd/cliproxy-plugin
go build -trimpath -buildvcs=false -o "$output_dir/doubao-helper" ./cmd/doubao-helper
if [[ $# -ge 2 ]]; then
 cpa_source=$2
 pinned=5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3
 if [[ $(git -C "$cpa_source" rev-parse HEAD) != "$pinned" ]]; then
  echo "CPA source must be at $pinned" >&2; exit 2
 fi
 if git -C "$cpa_source" apply --check "$project_root/patches/cpa-http-wire.patch" 2>/dev/null; then
  git -C "$cpa_source" apply "$project_root/patches/cpa-http-wire.patch"
 elif ! git -C "$cpa_source" apply --reverse --check "$project_root/patches/cpa-http-wire.patch" 2>/dev/null; then
  echo 'Host patch conflicts; checkout a clean pinned revision' >&2; exit 2
 fi
 cd -- "$cpa_source"
 go test ./internal/pluginhost -run 'Test(PublicNetwork|PluginHTTPBridgeDisableRedirects)'
 go build -trimpath -buildvcs=false -o "$output_dir/cli-proxy-api" ./cmd/server
fi
