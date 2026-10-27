# 运行

在仓库根目录执行。密钥只放在环境变量里，下面的示例值都是假的。

## 环境变量

| 变量 | 作用 | 默认或示例 |
| --- | --- | --- |
| `URAG_ADDR` | 监听地址 | `:8080` |
| `URAG_DATA_DIR` | 本地对象目录 | `./data` |
| `URAG_STORAGE` | `local` 或 `s3` | `local` |
| `URAG_S3_ENDPOINT` | S3 兼容端点 | 假地址，仅 `s3` 时使用 |
| `URAG_S3_REGION` | 区域 | 假区域 |
| `URAG_S3_BUCKET` | 桶 | 假桶名 |
| `URAG_S3_ACCESS_KEY` | 访问密钥变量 | 假密钥 |
| `URAG_S3_SECRET_KEY` | 私密密钥变量 | 假密钥 |
| `URAG_S3_PATH_STYLE` | `1` / `true` / `TRUE` / `True` 时打开路径风格 | `true` |
| `URAG_EMBED_ENDPOINT` | 留空则用 64 维夹具嵌入；有值才用 HTTP 嵌入 | 假地址 |
| `URAG_EMBED_MODEL` | HTTP 嵌入的模型名 | 假模型名 |
| `URAG_EMBED_API_KEY` | HTTP 嵌入读取的密钥；程序只保存变量名 | 假密钥 |
| `URAG_EMBED_BATCH_SIZE` | 嵌入批大小 | `16` |
| `URAG_EMBED_CONCURRENCY` | HTTP 嵌入并发 | `2` |
| `URAG_CLEAN_CONCURRENCY` | 清洗并发上限 | `2` |
| `URAG_UPLOAD_CONCURRENCY` | 同时运行的上传任务数 | `2` |
| `URAG_PART_SIZE` | S3 分片大小，字节 | `5242880` |
| `URAG_RETRY_MAX` | 失败尝试次数，含第一次 | `3` |
| `URAG_VECTOR_WEIGHT` | 混合分数里的向量权重 | `0.7` |
| `URAG_KEYWORD_WEIGHT` | 混合分数里的关键词权重 | `0.3` |
| `URAG_CHUNK_MAX_LEN` | 片段最大字节 | `800` |
| `URAG_CHUNK_OVERLAP` | 相邻片段重叠字节 | `100` |
| `URAG_FIXTURE` | 召回夹具路径 | `testdata/recall/fixture.json` |
| `URAG_INLINE_JOBS` | `true` 时任务在接口返回前跑完 | `false` |
| `URAG_MYSQL_DSN` | `urag crawl` 的台账连接。没有它时收录命令退出 | 假 DSN，见 `.env.example` |
| `URAG_EMBED_MODEL_DIR` | 本地嵌入权重目录。目录不存在时收录命令退出，不下载 | 已有目录 |
| `URAG_COLLECTION` | 收录写入的集合 | `public` |

`URAG_STORAGE=s3` 且端点、桶或密钥缺失时，进程退出并提示，不会伪造客户端。未设置 `URAG_EMBED_ENDPOINT` 时不会访问嵌入服务。`urag crawl` 在缺少 `URAG_MYSQL_DSN` 或 `URAG_EMBED_MODEL_DIR` 时退出，不会下载权重。

占位示例见仓库根目录的 `.env.example`。不要把真实密钥写进这个文件。

## 测试

```bash
go test ./...
```

任务管道单独跑：

```bash
go test ./internal/job/
```

`go test ./...` 不访问外网，不连接 MySQL，也不下载模型。页面测试和 HTTP 来源使用 `httptest`，不监听真实端口。提问和评价仍读召回夹具，走内存索引和夹具嵌入。

## 收录

种子文件每行一个站点地图或目录页 URL。空行和以 `#` 开头的行略过。

```bash
go run ./cmd/urag crawl seeds.txt
```

命令要求 `URAG_MYSQL_DSN`。权重目录是 `URAG_EMBED_MODEL_DIR`，目录必须已经存在。同一主机相邻请求间隔 1 秒。向量写在 `URAG_DATA_DIR/vectors`。原文只留下来源 URL，不写入对象存储。成功时打印该集合的篇数，篇数来自 `catalog.CountIndexed`。

## 本地前向

`embed.NewQwen` 只读本机目录里的 `Qwen/Qwen3-Embedding-4B`。`config.json` 的 `hidden_size` 必须是 2560，并且目录里要有权重文件；否则返回错误，不下载，不降维，也不把 `Qwen3-Embedding-0.6B` 当成可用模型。建索引时只加载这个嵌入模型。每一批文本启动一次短生命周期的 `python3`：子进程读完这一批，在同一个 device 上做前向（本机有 MPS 就用 MPS，选中后不再换到 CPU），把 2560 维向量写回后退出。Go 不把整库向量留在内存里，一批超过 256 条会直接拒绝。文档按原文编码，不加指令。查询只使用已经拼好的 `embed.QueryText`。

重排模型是 `Qwen/Qwen3-Reranker-4B`，由 `NewQwenReranker` 另行加载。它只给调用方送来的段落打分，不查库，不生成答案。嵌入子进程退出之后，重排子进程才可以启动，两个模型不会同时常驻。指令与嵌入相同：`Instruct: Given a web search query, retrieve relevant passages that answer the query`，`Query:` 后面直接接问题。本机没有重排模型卡，程序不联网打开模型页。前向使用本机已有的 Python，还需要该环境里的 `torch` 和 `transformers`；程序不替你安装，也不下载权重。

## 验收页

```bash
go run ./cmd/urag serve
```

默认地址是 `:8080`。浏览器打开：

- http://127.0.0.1:8080/upload
- http://127.0.0.1:8080/fetch
- http://127.0.0.1:8080/clean
- http://127.0.0.1:8080/ask
- http://127.0.0.1:8080/eval

`GET /` 跳到 `/upload`。

上传页提交集合、幂等键和文件，页面上能看到对象键、校验和和任务编号。获取页可选本地目录或 URL。清洗页按集合显示正文，重复文档会标出指向。提问页按集合显示片段、分数、集合编号和 Start/End。评价页显示每个集合的 Recall@K 和 MRR，包括正常结果和对照。

对应 JSON：

- `POST /api/uploads`
- `POST /api/fetch`
- `GET /api/jobs/{id}`
- `GET /api/collections/{id}/documents`
- `POST /api/retrieve`
- `POST /api/eval` 和 `GET /api/eval/report`
