# 2026-10-27 收录命令

选题：把 docx 清洗、公开收录、MySQL 台账和磁盘向量接到 `urag crawl`。一期原文只留下来源 URL。

## 做了什么

- 按 `feat/docx`、`feat/crawl`、`feat/ledger`、`feat/vectors` 的顺序合进 main。
- `urag crawl` 从种子文件列出文档，用 `Open` 把响应流交给清洗，再写入台账和磁盘索引。
- 篇数等于 `catalog.CountIndexed`：清洗后不是重复，且至少有一个子块。不支持的类型、下载失败和重复文档不计入。没有子块的不计入。
- 没有 `URAG_MYSQL_DSN`，或 `URAG_EMBED_MODEL_DIR` 指向的目录不存在时，命令退出。测试不走这两条，也不下载模型。

## 改了哪些路径

- `cmd/urag/main.go`
- `internal/job/crawl.go`
- `internal/job/job.go`
- `docs/architecture.md`
- `docs/operations.md`
- `docs/modules.md`
- `AGENTS.md`
- `.env.example`
- `changelog/2026-10-27-crawl-command.md`

## 怎么验收

```bash
go test ./... -count=1 -timeout 180s
```

应当通过，包括原来的 Recall@5 和 MRR 阈值。篇数口径由 `TestCrawlCountIndexedMatchesQualifiedDocuments` 检查。没有 10 万篇语料上的召回数字。

## 未做 / 下一步不要做

- 不要把 `RetrieveChildren` 接到验收页，提问路径仍用现有夹具。
- 没有 10 万篇的报告，不要声称这次收录让召回变好。
