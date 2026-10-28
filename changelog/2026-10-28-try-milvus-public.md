# 2026-10-28 试查改读 Milvus

选题：试查页还在扫磁盘向量。本机 Milvus 的集合 `public` 已经有向量，磁盘上还有更新的子块。

## 做了什么

- `urag sync-disk` 把磁盘向量里 Milvus 还没有的 `chunk_id` 补进集合 `public`。已有主键跳过。向量用磁盘上的，不重新嵌入。
- `urag try` 的向量检索读 `URAG_MILVUS_ADDR` 上的集合 `public`。不再打开磁盘索引做检索。
- 没有 `URAG_ELASTIC_ADDR`，或 Elasticsearch 没连上时，页面日志写「BM25 未连接」。不把进程内 BM25 标成已经查过 Elasticsearch。
- 重排仍用 `URAG_RERANK_MODEL_DIR`。这次本机是 `bge-reranker-base`。

## 改了哪些路径

- `internal/index`
- `internal/retrieve`
- `internal/web`
- `cmd/urag`
- `docs/operations.md`
- `AGENTS.md`
- `changelog/2026-10-28-try-milvus-public.md`

## 怎么验收

```bash
go test ./internal/web/ -count=1
```

应当通过。不访问外网。

## 未做 / 下一步不要做

- 不要为了试查再开一个收录进程。
- Elasticsearch 没启动时不要在进程内算 BM25 再写成已经查过 ES。
