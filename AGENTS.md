# urag

这是独立的检索程序。不要做成聊天机器人。不要读取或导入 imagine、vgraph 的业务数据。

## 包边界

- `internal/model` 只有共享类型。
- `internal/storage` 流式存放原文。不清洗，不建索引，不导出云厂商客户端类型。
- `internal/acquire` 把来源收成原文。公开收录只列出文档并在 `Open` 时打开正文流，不把原文写入对象存储。数据库和厂商 API 只有构造函数，返回未实现。
- `internal/clean` 得到 UTF-8 正文和血缘。不切分，不嵌入。
- `internal/chunk` 切片段。不嵌入，不检索。
- `internal/embed` 把文本变成向量。
- `internal/index` 一个集合一个索引，没有搜索全部集合的方法。
- `internal/retrieve` 只检索点名的集合。
- `internal/eval` 计算 Recall@K 和 MRR，不改索引。
- `internal/job` 编排任务，不提供页面。
- `internal/web` 只做页面和 JSON。不算向量，不把上传正文读进内存。
- `cmd/urag` 只组装依赖。`serve` 监听验收页，`try` 打开本机试查页。向量检索读 Milvus 里的集合 public，ef 大于本次 k。BM25 读 Elasticsearch 索引 public。嵌入模型在试查进程里只加载一次，重排用完退出。Elasticsearch 没连上时页面写 BM25 未连接。`crawl` 按种子文件收录。`index` 把子块写入 Milvus。`load-bm25` 只把 Milvus 里已有子块的正文写入该集合的 Elasticsearch 索引，不嵌入。`search` 在点名集合里做向量和 BM25 检索。

## 测试

`go test` 不访问外网。页面测试用 `httptest`，不监听真实端口。HTTP 来源只用 `httptest`。

## 密钥

密钥只来自环境变量。不要写进仓库、日志或文档。示例只用假值。
