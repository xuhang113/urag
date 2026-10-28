#!/usr/bin/env bash
# 创建或补写 Elasticsearch 索引 public（smartcn），并写入同一份夹具。
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/urag init-es
