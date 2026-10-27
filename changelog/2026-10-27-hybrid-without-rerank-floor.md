# 2026-10-27 没有重排分时不套 0.5

选题：试查页在 Qwen 重排没有加载时，把 `URAG_RERANK_MIN_SCORE` 用在了混合分上。中文查询因此被滤成 0 条。

## 做了什么

- 只有重排器真正返回分数时，试查才按 `URAG_RERANK_MIN_SCORE`（默认 0.5）丢掉低分。
- 嵌入进程还在、没有重排分时，按混合分返回最多 8 条，每条仍显示分数。页面写「本次没有重排分，未按 0.5 过滤」。
- 步骤的开始、结束、耗时，常驻嵌入，以及 3 分钟超时都还在。`urag search` 仍是先重排再套门槛。

## 改了哪些路径

- `internal/retrieve`
- `internal/job`
- `internal/web`
- `cmd/urag/try_cmd.go`
- `docs/operations.md`
- `changelog/2026-10-27-hybrid-without-rerank-floor.md`

## 怎么验收

```bash
go test ./internal/web/ -count=1
```

应当通过。不访问外网，不加载真实 4B。

## 未做 / 下一步不要做

- 不要为了试查去停正在跑的收录，也不要新开收录。
- 不要在没有重排分时用 0.5 滤混合分。
- 不要把重排模型和嵌入模型一起留在内存里。
