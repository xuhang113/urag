# 2026-10-27 重排分数门槛

选题：试查页对不相干的问题也会把重排后的前 8 条填满。低分结果不该返回。

## 做了什么

- 试查页和 `urag search` 只返回高于 `URAG_RERANK_MIN_SCORE` 的重排命中。未设置时默认 `0.5`。Qwen rerank 分数，低于此视为不相关。
- 不够门槛的丢掉，可以返回 0 条。页面上 0 条时写「没有足够相关的片段」。
- 夹具评价仍走原来的排序，不套这道门槛。`testdata/recall/fixture.json` 没有改。

## 改了哪些路径

- `internal/retrieve`
- `internal/job`
- `internal/web`
- `cmd/urag`
- `docs/operations.md`
- `.env.example`
- `changelog/2026-10-27-rerank-min-score.md`

## 怎么验收

```bash
go test ./internal/retrieve/ ./internal/web/ ./internal/job/ ./internal/eval/ ./cmd/urag/ -count=1
```

应当通过。不访问外网，不加载真实 4B。

## 未做 / 下一步不要做

- 不要把门槛放进 `Retrieve` 或夹具 Recall。
- 不要用低分结果补满 8 条。
- 不要动正在跑的收录进程。
