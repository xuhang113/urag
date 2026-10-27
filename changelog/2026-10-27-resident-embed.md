# 2026-10-27 收录期间嵌入模型常驻

选题：每 16 条子块就重新启动 `qwen_forward.py`，Qwen3-Embedding-4B 被反复加载，收录太慢。

## 做了什么

- `urag crawl` 在整次收录开始时启动一个嵌入进程。之后按批从 stdin 送文本，stdout 回 2560 维向量。收录结束、失败或取消时关掉它。
- 模型只加载一次，留在启动时选定的 device 上。本机有 MPS 就用 MPS 和 float16，中途不换到 CPU。
- 批大小仍是 `URAG_EMBED_BATCH_SIZE`，默认改为 64，最大 256。不把整库放进内存。
- 重排仍用重叠重排，不加载重排模型。原文仍不写入对象存储。

## 改了哪些路径

- `internal/embed/qwen.go`
- `internal/embed/qwen_forward.py`
- `internal/embed/resident.go`
- `internal/embed/forward.go`
- `internal/embed/qwen_test.go`
- `internal/job/job.go`
- `internal/job/crawl.go`
- `internal/job/crawl_test.go`
- `cmd/urag/main.go`
- `cmd/urag/main_test.go`
- `docs/operations.md`
- `.env.example`
- `changelog/2026-10-27-resident-embed.md`

## 怎么验收

```bash
go test ./internal/embed/ ./internal/job/ ./cmd/urag/ -count=1
```

应当通过。测试用假命令，不加载真实 4B，不访问外网。`TestQwenResidentReusesOneProcess` 检查多批共用一个进程，关掉之后才退出。

## 未做 / 下一步不要做

- 没有重跑召回评价，不要声称召回变好。
- 不要把重排模型和嵌入模型一起加载。
- 不要把原文写入对象存储。
