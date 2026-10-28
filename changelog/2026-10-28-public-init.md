# 2026-10-28 公开仓库的一键初始化

选题：克隆之后需要能拉起 MySQL、Milvus、Elasticsearch，并准备模型、台账夹具和索引 public。

## 做了什么

- `scripts/bootstrap.sh` 复用容器 `urag-mysql`、`urag-milvus`、`urag-elastic`，跑三项初始化，再执行 `go test ./...`。不启动收录。
- `scripts/init-models.py` 按环境变量或默认目录下载并校验 `Qwen3-Embedding-4B` 与 `BAAI/bge-reranker-base`。重排目录名保持 `bge-reranker-base`。
- `urag init-db` 执行已有 `schema.sql`，并写入 `testdata/bootstrap/fixture.json`。只替换 `fixture://urag/` 这几行。
- `urag init-es` 在缺索引时按 smartcn 和 BM25 创建索引 `public`，并写入同一份夹具。已有索引不重建。
- 根目录 README 用中文说明功能、部署和检索流程。

## 改了哪些路径

- `scripts/`
- `deploy/`
- `cmd/urag`
- `internal/catalog`
- `testdata/bootstrap/fixture.json`
- `README.md`
- `docs/operations.md`
- `AGENTS.md`
- `.env.example`
- `.gitignore`

## 怎么验收

```bash
./scripts/bootstrap.sh
```

应当复用已有容器，校验本机模型，写入夹具，并让 `go test ./...` 通过。

## 未做 / 下一步不要做

- 不要把权重、向量、原文或库转储提交进 git。
- 不要新开收录。
- 不要删除 MySQL 或 Milvus 里已有的生产数据。
