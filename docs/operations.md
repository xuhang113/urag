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
| `URAG_EMBED_BATCH_SIZE` | 嵌入批大小 | `64` |
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
| `URAG_RERANK_MIN_SCORE` | 真正拿到重排分之后的门槛。试查页没有重排分时不使用它，按混合分最多返回 8 条。`urag search` 先重排再套这道门槛。低于此视为不相关，可以是 0 条。夹具评价不使用它 | `0.5` |
| `URAG_RERANK_MODEL_DIR` | 试查重排权重目录。未设置时用嵌入目录旁边的 `Qwen3-Reranker-4B`。目录是 sequence-classification cross-encoder 时用小模型打分，否则仍用 Qwen3-Reranker-4B。目录不存在时退出，不下载 | 已有目录 |

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

命令要求 `URAG_MYSQL_DSN`。权重目录是 `URAG_EMBED_MODEL_DIR`，目录必须已经存在。同一主机相邻请求间隔 1 秒。向量写在 `URAG_DATA_DIR/vectors`。原文只留下来源 URL，不写入对象存储。已完成的 source URI 写在 `URAG_DATA_DIR/crawl-checkpoint.json`。进程中断后再次执行同一命令，会跳过这些地址。种子按每 8 条一批列入并入库，每批结束后向标准输出打印当前篇数。标准错误打印这一批的跳过原因计数，以及抽不出文字的 PDF 数。抽不出文字的 PDF 写入断点后继续，不中断后面的种子。成功结束时再打印该集合的篇数，篇数来自 `catalog.CountIndexed`。错误文本里的数据库连接串会抹掉。

## 本地前向

`embed.NewQwen` 只读本机目录里的 `Qwen/Qwen3-Embedding-4B`。`config.json` 的 `hidden_size` 必须是 2560，并且目录里要有完整权重和分词器；否则返回错误，不下载，不降维，也不把 `Qwen3-Embedding-0.6B` 当成可用模型。`URAG_EMBED_MODEL_DIR` 可以是快照目录，也可以是 Hugging Face 缓存根目录 `models--Qwen--Qwen3-Embedding-4B`。程序只打开本机 `refs/main` 指向的快照；断掉的符号链接、缺分片或没有分词器都直接失败，不补下载。建索引时只加载这个嵌入模型。`urag crawl` 在整次收录开始时启动一个 `python3`，进程保持到收录结束、失败或取消。每一批文本从 stdin 送进去，stdout 回一行 2560 维向量。模型只在第一批加载，留在启动时选定的 device 上（本机有 MPS 就用 MPS 和 float16，中途失败也不换到 CPU）。Go 不把整库向量留在内存里。批大小是 `URAG_EMBED_BATCH_SIZE`，默认 64，最大 256。文档按原文编码，不加指令。查询只使用已经拼好的 `embed.QueryText`。原文不写入对象存储。收录不加载重排模型。

重排模型默认是 `Qwen/Qwen3-Reranker-4B`，由 `NewQwenReranker` 另行加载。它的目录同样可以是缓存根目录 `models--Qwen--Qwen3-Reranker-4B`。它只给调用方送来的段落打分，不查库，不生成答案。收录期间嵌入子进程一直占着模型闸。它退出之后，重排子进程才可以启动，两个模型不会同时常驻。指令与嵌入相同：`Instruct: Given a web search query, retrieve relevant passages that answer the query`，`Query:` 后面直接接问题。本机没有重排模型卡，程序不联网打开模型页。前向使用本机已有的 Python，还需要该环境里的 `torch` 和 `transformers`；程序不替你安装，也不下载权重。试查页可以用 `URAG_RERANK_MODEL_DIR` 指向本机已有的 cross-encoder 目录，例如 `BAAI/bge-reranker-base`。未设置时仍加载 `Qwen3-Reranker-4B`。程序不下载这个小模型。

## 生产检索

`urag crawl` 继续把向量写在磁盘索引里。生产向量库是 Milvus。

```bash
go run ./cmd/urag index
go run ./cmd/urag search 问题
```

两条命令都读取 `URAG_MILVUS_ADDR`。没有这个地址就退出，不会连接猜出来的主机或端口。示例只在 `.env.example` 里，值是假的。

`index` 只读取台账里该集合的子块。嵌入文本是 `chunk.Passage`，文档侧不加指令。向量、`chunk_id`、`parent_id`、`doc_id`、`version`、`collection_id`、`heading_path`、`page` 和子块正文写入这一个 Milvus collection。不写 PDF 或 DOCX 原文，不把 BM25 写进 Milvus，也不打开别的集合。建索引只加载嵌入模型，不加载重排模型，不读种子，不改断点。

`search` 只打开 `URAG_COLLECTION`。向量 top 200 和该集合关键词索引上的 BM25 top 200 合并后，只把前 50 个子块送给 `Qwen/Qwen3-Reranker-4B`。重排只出分数。分数必须高于 `URAG_RERANK_MIN_SCORE` 才留下，最多 8 条，可以一条都没有；再按 `parent_id` 从台账取父块整节。查询使用 `embed.QueryText`。嵌入进程关掉之后才启动重排进程。关键词索引由台账里的子块正文在内存里重建，不另起搜索引擎。夹具评价仍按原排序算 Recall，不套这道门槛。

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
- `POST /api/try`
- `POST /api/eval` 和 `GET /api/eval/report`

## 试查

`urag try` 只打开集合 `public`。向量检索读 `URAG_MILVUS_ADDR` 上的这个 Milvus collection。没有这个地址就退出，不猜地址，也不改读磁盘索引。不新建向量文件，也不启动收录。Elasticsearch 没启动，或没有 `URAG_ELASTIC_ADDR` 时，页面日志写「BM25 未连接」，不用进程内 BM25 冒充已经查过 Elasticsearch。重排仍用 `URAG_RERANK_MODEL_DIR`。

```bash
go run ./cmd/urag try
```

嵌入权重是 `URAG_EMBED_MODEL_DIR`。重排权重优先用 `URAG_RERANK_MODEL_DIR`，否则找嵌入目录旁边的 `Qwen3-Reranker-4B`。目录不存在就退出，不下载。页面写出这次的模型名和目录。日志里的「重排模型 …」一行带开始时间、结束时间和耗时。只有真正拿到重排分时才按 `URAG_RERANK_MIN_SCORE` 过滤。

页面监听 `127.0.0.1:8080`。这个端口已被占用时改用 `127.0.0.1:8088`。浏览器打开 `/try`。`POST /api/try` 的 JSON 只有 `query`。Milvus 向量和已连接的 BM25 合并后留下前 50 条。Elasticsearch 没连上时，这一步是「BM25 未连接」，合并只用向量命中。拿到重排分之后，只返回分数高于 `URAG_RERANK_MIN_SCORE` 的命中，最多 8 条。没有达到门槛时页面写「没有足够相关的片段」，不用低分结果填满 8 行。没有重排分时不套这道门槛，按混合分返回最多 8 条，每条带分数，页面写「本次没有重排分，未按 0.5 过滤」。响应里的 `chunk` 是子块，`parent` 是父块正文；父块查不到时没有 `parent`。原文位置是 `start_byte` 和 `end_byte`。不带 `Accept: text/event-stream` 时响应仍是命中 JSON 数组。

试查进程启动时加载一次嵌入模型，并留到进程结束。后面的提问不再重新加载，页面日志写「嵌入模型已在内存」。一次提问结束不退出这个进程。重排模型若单独运行，仍然每次用完就退出，并且不和嵌入模型同时留在内存里。重排前记下嵌入进程；嵌入进程还在时不再启动重排模型，这时没有重排分，按混合分返回。空闲内存不够再放一个约 7.5GB 的模型时，第一次在页面日志写「为重排暂时卸下嵌入」。查询向量继续用常驻嵌入，避免每次提问都重新加载权重。

页面用同一次请求的服务器推送，按这个真实顺序显示步骤。每一行有本机本地时间的开始、结束和耗时，时间精确到秒，耗时是整秒。正在进行的一步有开始时间，没有结束时间，并标成「未完成」；没做完不会被标成完成。嵌入进程还在、没有重排分时，第一次提问的顺序是：加载嵌入模型、计算查询向量、向量检索、BM25、合并前 50 条、为重排暂时卸下嵌入、取父块。结果注明「本次没有重排分，未按 0.5 过滤」。真正拿到重排分时才多出「重排」和「按 0.5 分过滤」，过滤使用 `URAG_RERANK_MIN_SCORE`。之后的提问用「嵌入模型已在内存」代替「加载嵌入模型」，并且不再重复卸下嵌入。嵌入或重排子进程若 3 分钟内没有返回，这次请求结束，页面显示「检索超时。收录正在占用嵌入模型，这次没有结果。」拿到重排分但没有达到门槛时页面写「没有足够相关的片段」。已设置 `URAG_MYSQL_DSN` 时用它读取父块，错误文本不带连接串。没有这个变量时只显示子块。原有 `/ask` 仍在。`Accept: text/event-stream` 时 `POST /api/try` 返回事件流；不带这个头时仍返回命中 JSON。
