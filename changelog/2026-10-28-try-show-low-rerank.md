# 2026-10-28 试查保留低分重排

选题：`bge-reranker-base` 的分数常常低于 0.5。试查按这个门槛滤掉全部命中，页面只剩「没有足够相关的片段」。

## 做了什么

- 试查在重排之后不再按 `URAG_RERANK_MIN_SCORE` 丢掉命中。按分数从高到低返回最多 8 条，每条显示原始分。
- 最高分低于 0.5 时，页面写「最高分低于 0.5，仍显示前几条供查看」。
- `urag search` 仍按门槛丢掉低分。

## 改了哪些路径

- `internal/job/try.go`
- `internal/retrieve`
- `internal/web`
- `docs/operations.md`
- `changelog/2026-10-28-try-show-low-rerank.md`

## 怎么验收

```bash
go test ./internal/web/ -count=1
```

应当通过。不访问外网。

## 未做 / 下一步不要做

- 不要为了这次试查再开收录。
- 不要把 `urag search` 的 0.5 门槛一起拿掉。
