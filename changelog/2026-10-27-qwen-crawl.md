# 2026-10-27 本地嵌入与可断点收录

选题：把 Qwen 前向合进 main，收录建索引只用嵌入模型，并把已完成的来源地址写到磁盘。

## 做了什么

- 将 `feat/qwen-runner` 快进合入本地 main。嵌入是短生命周期的本机 Python 进程，维度保持 2560。
- `urag crawl` 继续调用 `embed.NewQwen`。重排仍用可手算的重叠重排，不同时加载重排模型。
- 已完成的 source URI 写入 `URAG_DATA_DIR/crawl-checkpoint.json`。进程中断后再次执行，会跳过这些地址。
- 原文仍不写入对象存储。篇数仍是清洗后不是重复、且至少有一个子块的文档。

## 改了哪些路径

- `internal/job/job.go`
- `internal/job/crawl.go`
- `internal/job/crawl_test.go`
- `cmd/urag/main.go`
- `docs/architecture.md`
- `docs/operations.md`
- `changelog/2026-10-27-qwen-crawl.md`

## 怎么验收

```bash
go test ./... -count=1 -timeout 180s
```

应当通过，且不加载真实 4B。`TestCrawlCheckpointFileSkipsCompletedSources` 检查断点文件能让下一次收录不再请求已完成的来源。

## 未做 / 下一步不要做

- 没有在这次样本上重跑召回评价，不要声称召回变好。
- 不要把重排模型和嵌入模型一起加载。
- 不要把原文写入对象存储。
