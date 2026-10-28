# 2026-10-28 试查重排目录可配置

选题：试查页重排用 Qwen3-Reranker-4B，加载太慢。本机缓存里没有更小的 cross-encoder。

## 做了什么

- 试查页和 `urag try` 读取 `URAG_RERANK_MODEL_DIR`。未设置时仍用嵌入目录旁边的 `Qwen3-Reranker-4B`。
- 目录是 sequence-classification cross-encoder 时，用小模型给段落打分。否则仍走 Qwen 重排。权重不在本机就不下载。
- 页面写出这次的模型名和目录。日志里的「重排模型 …」带开始时间、结束时间和耗时，标准错误另有一行模型名和秒数。
- 0.5 分门槛仍只在真正拿到重排分时使用。步骤起止时间和 3 分钟超时仍在。夹具阈值没改。

## 改了哪些路径

- `internal/retrieve`
- `internal/web`
- `cmd/urag`
- `docs/operations.md`
- `.env.example`
- `changelog/2026-10-28-small-rerank-dir.md`

## 怎么验收

```bash
go test ./internal/web/ -count=1
```

应当通过。不访问外网，不加载真实 4B。

## 未做 / 下一步不要做

- 本机没有 `bge-reranker-base` 或 `bge-reranker-v2-m3`。不要为了试查去下载 4B 或别的大文件。
- 小模型放到本机目录后，把 `URAG_RERANK_MODEL_DIR` 指过去再重启试查。建议目录是 `~/.cache/urag-models/bge-reranker-base`，模型是 `BAAI/bge-reranker-base`。
- 不要改召回夹具，也不要动正在跑的收录。
