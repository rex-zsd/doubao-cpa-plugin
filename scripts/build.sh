#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${1:?Usage: build.sh ABS_OUTPUT_DIR}
case "$output_dir" in /*) ;; *) echo 'Output directory must be absolute' >&2; exit 2;; esac
mkdir -p -- "$output_dir"
cd -- "$project_root"
export CGO_ENABLED=1
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.0}
go test -race ./...
go build -trimpath -buildvcs=false -buildmode=c-shared -o "$output_dir/doubao-work.so" ./cmd/cliproxy-plugin
# The helper runs inside the CPA container or on the host; keep it static.
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$output_dir/doubao-helper" ./cmd/doubao-helper
