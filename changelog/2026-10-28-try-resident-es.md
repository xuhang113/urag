# 2026-10-28 试查常驻嵌入并接通 Elasticsearch

选题：试查每次提问都重新加载 Qwen3-Embedding-4B。关键词分还写着「BM25 未连接」。

## 做了什么

- `urag try` 在进程里启动一次嵌入进程，提问结束不关掉它。第二次提问日志写「嵌入模型已在内存」。
- 启动后放开模型闸。重排仍用 `URAG_RERANK_MODEL_DIR` 里的模型，这次是 `bge-reranker-base`，用完退出。闸还被占着时才跳过重排。
- 集合 `public` 的 Elasticsearch 索引名就是 `public`。字段是 `chunk_id`、`parent_id`、`doc_id`、`collection_id`、`heading_path`、`body`，中文分析器是 smartcn。
- `urag load-bm25` 把台账子块写入这个索引。不嵌入，不写向量。
- 试查的 BM25 走这个索引。向量仍走 Milvus，ef 大于本次 k。没连上时日志仍写「BM25 未连接」。
- 测试用假会话，不连接真实 Elasticsearch。

## 改了哪些路径

- `cmd/urag`
- `internal/embed`
- `internal/job`
- `internal/index`
- `internal/web`
- `docs/operations.md`
- `AGENTS.md`
- `changelog/2026-10-28-try-resident-es.md`

## 怎么验收

```bash
go test ./internal/web/ ./internal/index/ -count=1
```

应当通过。不访问外网，不连接真实 Elasticsearch。

## 未做 / 下一步不要做

- 不要新开收录。
- 不要删 Milvus 或 MySQL 里的数据。
- 不要把重排模型留在内存里。
- 不要为了灌关键词而重新嵌入。
