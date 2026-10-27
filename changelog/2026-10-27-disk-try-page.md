# 2026-10-27 磁盘向量试查页

选题：已经写进磁盘的向量需要一个本机页面试查，不改正在跑的收录，也不接 Milvus。

## 做了什么

- `GET /try` 和 `POST /api/try` 加在现有页面里。`/ask` 还在。
- 试查只打开集合 `public`。磁盘向量和该集合 BM25 合并后留下前 50 条，重排后最多 8 条。
- 页面和 JSON 显示子块、分数、`collection_id`、`heading_path`、原文位置。查到父块时另给父块正文。
- 查询向量返回前关掉嵌入模型，然后再加载重排。两个权重不同时常驻。
- `urag try` 监听 `127.0.0.1:8080`，占用时改用 `127.0.0.1:8088`。不新建向量文件，不写连接串。

## 改了哪些路径

- `internal/retrieve`
- `internal/job`
- `internal/web`
- `cmd/urag`
- `docs/operations.md`
- `AGENTS.md`
- `changelog/2026-10-27-disk-try-page.md`

## 怎么验收

```bash
go test ./internal/web/ ./internal/retrieve/ -count=1
```

应当通过。不访问外网，不加载真实 4B。

## 未做 / 下一步不要做

- 没有重跑召回评价，不要声称召回变好。
- 不要把重排模型和嵌入模型一起常驻。
- 不要把原文写入对象存储，也不要动正在跑的收录进程。
