# 2026-10-27 Milvus 向量加 BM25 重排

选题：生产检索还在扫磁盘向量，企业库是 Milvus。检索要先向量和本集合 BM25，再只重排前 50 个子块。

## 做了什么

- 新增 Milvus 适配器。一个集合一个 collection。业务仍用 index 和 retrieve 的接口，导出签名里没有 Milvus SDK 类型。
- `urag index` 只嵌入子块并写入 Milvus。只加载嵌入模型。没有 `URAG_MILVUS_ADDR` 就退出，不猜地址。
- `urag search` 在点名集合里做向量 topK 和 BM25 topK，合并后只把 50 条子块送给重排，留下最多 8 条，再取父块正文。
- 磁盘索引和 `urag crawl` 保持原样。测试用假向量库和内存关键词索引，不连接真实 Milvus。

## 改了哪些路径

- `internal/index`
- `internal/retrieve`
- `internal/catalog`
- `internal/job`
- `cmd/urag`
- `.env.example`
- `docs/operations.md`
- `docs/architecture.md`
- `docs/modules.md`
- `specs/recall.md`
- `AGENTS.md`
- `changelog/2026-10-27-milvus-hybrid.md`

## 怎么验收

```bash
go test ./internal/index/ ./internal/retrieve/ ./internal/embed/ ./internal/eval/ -count=1
```

应当通过。不访问外网，不要求安装 Milvus。夹具 Recall@5 和 MRR 的阈值不变。

## 未做 / 下一步不要做

- 没有把正在跑的 crawl 切到 Milvus，不要改它的断点或种子。
- 没有重跑公开语料的召回评价，不要声称召回变好。
- 不要把重排模型和嵌入模型一起常驻，也不要把原文写入对象存储或 Milvus。
