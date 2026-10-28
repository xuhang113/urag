#!/usr/bin/env bash
# 执行 schema.sql，并写入可重复的检索夹具。不收录，不打印连接串。
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/urag init-db
