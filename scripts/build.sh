#!/usr/bin/env bash
# Builds the WASI module the till executes in-process (ADR-0001).
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS=wasip1 GOARCH=wasm go build -o bin/plugin.wasm ./src
echo "built bin/plugin.wasm ($(wc -c < bin/plugin.wasm | tr -d ' ') bytes)"
