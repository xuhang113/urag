# 模块

每个 `internal` 包只做一件事。调用方向向下，页面不绕过任务直接碰存储或索引。

| 包 | 负责 | 不负责 | 依赖 |
| --- | --- | --- | --- |
| `model` | 集合、文档、片段、血缘这些共享类型 | 存储、清洗、切分、嵌入、索引、检索、HTTP | 无 |
| `storage` | 按对象键流式存放原文，校验和、幂等、分片、有限次退避 | 解析正文、清洗、建索引；不导出云厂商客户端类型 | `model` |
| `acquire` | 把来源列成条目。上传和抓取把正文流式交给存储；公开收录只列出文档，`Open` 再打开正文流 | 清洗、切分、嵌入；收录不把原文写入对象存储 | `model`、`storage` |
| `clean` | 识别类型、转成 UTF-8、去掉样板、在集合内去重 | 切分、嵌入、建索引；不去重其他集合 | `model` |
| `chunk` | 把一篇文档切成带字节区间的片段 | 嵌入、检索；不把不同集合切到一起 | `model` |
| `embed` | 把文本变成向量 | 切分、建索引；密钥不写入日志 | 仅标准库 |
| `index` | 一个集合一个向量索引和关键词索引。生产向量库是 Milvus，磁盘索引留给测试和收录 | 没有搜索全部集合的方法；不嵌入文本；不把 Milvus 客户端类型放进导出签名 | `model`、`embed` |
| `retrieve` | 只在点名集合上召回、混合分数、可选重排 | 不打开未点名的集合；不清洗 | `model`、`embed`、`index` |
| `eval` | 夹具类型，以及 Recall@K 和 MRR | 不改写索引；不决定命中顺序 | `model`、`retrieve` |
| `catalog` | 用来源、父级章节、子块三张表记录清洗后的 UTF-8 和来源 URL | 不保存原文字节，不保存向量，不读取环境变量 | 仅标准库和 MySQL 驱动 |
| `job` | 上传、抓取、收录、清洗、嵌入、检索和评价任务 | 不提供 HTTP 页面；收录不把原文写入对象存储 | 上表除 `web` 以外的包 |
| `web` | 服务端 HTML 和 JSON API | 不算向量；不把上传正文读进内存；不调用 `RetrieveChildren` | `model`、`job`、`retrieve`、`eval` |

`cmd/urag` 从环境变量组装依赖。`serve` 调用 `web.Listen`。`crawl` 在有 `URAG_MYSQL_DSN` 和已存在的 `URAG_EMBED_MODEL_DIR` 时收录，目录缺失则退出，不下载权重。`index` 和 `search` 还要 `URAG_MILVUS_ADDR`，没有这个地址就退出，不连接猜出来的 Milvus。

## 只有接口的来源

`acquire` 里下面两种来源只有构造函数，调用返回 `ErrNotImplemented`，不是可用实现：

- 数据库：`NewDatabase`
- 厂商 API：`NewVendorAPI`

`NewSitemap` 解析站点地图。本地目录（`NewLocalDir`）和 HTTP（`NewHTTP`）是获取任务会调用的两种来源。数据库和厂商 API 不在这次的执行路径里接通。
