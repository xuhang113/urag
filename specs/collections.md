# 集合隔离

## 要做什么

一份文档只属于一个集合。一个集合一个 `Index`。查询必须在 `retrieve.Request.Collections` 或 `index.Query.CollectionID` 里点名集合。检索只打开点名的集合，各自召回后再按分数合并。每条 `retrieve.Hit` 都带 `CollectionID`。

夹具里的 gamma 只有干扰文档 `gamma-secret`。没有任何题目查询 gamma。

## 不做什么

不提供搜索全部集合的类型或函数。不打开请求未点名的集合。不把 gamma 的正文混进 alpha 或 beta 的命中。集合列表为空时返回 `retrieve.ErrCollectionsRequired`。

## 涉及的包

`internal/index` 依赖 `internal/model` 与 `internal/embed`。`internal/retrieve` 依赖 `internal/model`、`internal/embed`、`internal/index`。

## 数据格式

`index.Query.CollectionID` 必须等于该索引的集合，否则搜索返回错误。关键词分数为 `|queryTokens ∩ chunkTokens| / |queryTokens|`，查询无词时为 0。向量分数是余弦相似度。

`retrieve.Request` 的 JSON 字段是 `collections`、`query`、`top_k`、`hybrid`、`rerank`。`retrieve.Hit` 的 JSON 字段含 `collection_id`、`document_id`、`chunk_id`、`text`、`score`、`start_byte`、`end_byte`。

Hybrid 分数 = Vector 权重 × 余弦 + Keyword 权重 × 重叠。权重都为 0 时使用 0.7 和 0.3。

## 测试

### T4.1 TestUnnamedCollectionNotSearched

索引 alpha、beta、gamma 三份夹具文档。查询只点名 alpha 和 beta。断言命中的文档编号和正文都不来自 gamma，响应里不出现 `gamma only token`。包装 `Registry.Open`，断言没有打开 gamma。

### T4.2 TestCrossCollectionHitsCarryID

同一次查询点名 alpha 和 beta，问题使用 `m-lantern` 与 `m-harbor`。断言每条命中的 `CollectionID` 不是空，且等于该文档所属集合。alpha 的命中带 `alpha`，beta 的命中带 `beta`。
