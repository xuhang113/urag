# 召回评价

## 要做什么

用 `testdata/recall/fixture.json` 评价召回。K 为 5。阈值是 `normal_recall_at_k` 0.8、`normal_mrr` 0.6、`contrast_recall_at_k_max` 0.4。低于普通阈值，或对照 Recall@K 高于 0.4，测试失败。

`RecallAtK` 对每题先按 `DocumentID` 去重，保留第一次出现，不按分数重排。相关文档至少一篇出现在前 K 名则该题记 1，否则记 0，再对题目宏平均。`MeanReciprocalRank` 取第一个相关文档名次的倒数，名次从 1 开始，没有命中记 0，再宏平均。

只含一个集合的题计入该集合。多个集合的题只计入 `collection_id` 为 `merged` 的分数。题目没有集合，或题目数与排名数不一致时返回错误，不返回部分分数。

改切分、权重或重排时，与 `paragraph` 基线比。`Report.Direction` 取 up、down 或 flat。`RecallDelta` 与 `MRRDelta` 是全部题目的宏平均减去基线。没有基线时 `Direction` 为 `baseline`，两个 delta 为 0。没有报告不能声称召回变好。

默认切分名称是 `paragraph`。对照切分名称是 `drop_keywords`，丢掉 `contrast_drop_terms` 里的词。

## 不做什么

不把 gamma 编进任何查询。不把多集合题算进 alpha 或 beta 的单集合分数。不在没有 `Report` 时声称召回上升。`Load` 接通前返回 `ErrNotImplemented`。评价不改写索引，命中顺序由检索传入。

## 涉及的包

`internal/eval` 依赖 `internal/model` 与 `internal/retrieve`。夹具路径是 `testdata/recall/fixture.json`。对照切分来自 `internal/chunk`。

## 数据格式

JSON 使用 snake_case。顶层字段：`k`、`thresholds`、`contrast_drop_terms`、`collections`、`questions`。

阈值字段：`normal_recall_at_k`、`normal_mrr`、`contrast_recall_at_k_max`。

题目字段：`id`、`collections`、`query`、`relevant_docs`。

集合 alpha 含 `alpha-lantern`、`alpha-canal`、`alpha-library`。beta 含 `beta-harbor`、`beta-market`。gamma 含 `gamma-secret`，只作干扰文档。

单集合题：`q-lantern`、`q-canal`、`q-library`、`q-harbor`、`q-market`。查询使用对应文档里的独特词，`relevant_docs` 只有对应编号。

合并题：`m-lantern`、`m-harbor`。`collections` 为 `["alpha","beta"]`，相关文档分别是 `alpha-lantern` 与 `beta-harbor`。

`contrast_drop_terms` 依次为：crimson、lantern、festival、amber、canal、barge、municipal、library、catalogs、silver、harbor、crane、violet、night、market。

`Score` 字段：`collection_id`、`recall_at_k`、`mrr`、`k`。`RecallAtK` 填写前三项中的召回和 K，MRR 为 0。`MeanReciprocalRank` 填写集合编号和 MRR，召回与 K 为 0。

函数：

```go
func Load(path string) (Fixture, error)
func RecallAtK(questions []Question, hits [][]retrieve.Hit, k int) ([]Score, error)
func MeanReciprocalRank(questions []Question, hits [][]retrieve.Hit) ([]Score, error)
```

`hits[i]` 是第 i 题的名次，下标 0 是第 1 名。

## 测试

### T5.1 TestFixtureRecallAndMRR

用 paragraph 切分、默认权重 0.7/0.3、K=5 跑完全部题目。断言 alpha 与 beta 的 Recall@K 都不低于 0.8，MRR 都不低于 0.6。merged 出现在分数里。任一普通集合低于夹具阈值则失败。阈值从夹具读取，测试不得另写更低的数。

### T5.2 TestContrastSplitterWorse

用 `contrast_drop_terms` 构造 `drop_keywords` 切分，同一套题目再评价一次。断言每个单集合的 Recall@K 都不高于 0.4。高于 `contrast_recall_at_k_max` 则失败。与 paragraph 基线相比，报告 `Direction` 为 down。没有这份报告，不得声称召回变好。

## 查询前缀、磁盘索引和子块重排

夹具评价仍走内存索引、`embed.NewFixture` 和 `Retrieve`。不改 `testdata/recall/fixture.json`，也不把夹具向量改成 2560 维。`Retrieve` 的查询文本不加指令，Recall@5 和 MRR 的算法与阈值不变。

查询侧前缀是 `embed.QueryInstruct`：

```text
Instruct: Given a web search query, retrieve relevant passages that answer the query
Query:
```

`QueryText` 把前缀和问题直接拼在一起，`Query:` 后面不加空格。文档侧 `Embed` 不加这段指令。

`NewQwen` 只读本地权重。目录不存在，或维度不是 2560，就返回错误。不下载，也不改维度。模型名是 `Qwen/Qwen3-Embedding-4B`，维度是 2560。

`NewDisk` 一个集合一个目录，按批写入，搜索时不把全部向量读进内存。元数据记下模型名和维度。打开时不一致就返回错误，禁止用旧向量搜索。分数小于等于 0 的不返回。关键词索引仍是这个集合自己的。未点名的集合不打开，也搜不到。

`Hit` 增加 `parent_id` 和 `heading_path`。原有字段保留。

`RetrieveChildren` 只打开请求里点名的集合。混合召回先取前 50 个子块，只把这 50 对送去重排：查询是 `QueryText(问题)`，子块嵌入文本是 `heading_path + "\n" + body`，标题路径为空时只有 body。重排只产出分数，不生成答案。留下最多 8 条；不足 8 条就全留。然后按 `parent_id` 取父块整节并替换 `Text`。`parents` 为空或查不到时，`Text` 保持子块 body。每条命中都带 `collection_id`、`document_id`、`chunk_id`、`score`、`start_byte`、`end_byte`、`parent_id`、`heading_path`。

`NewQwenReranker` 在目录不存在时返回错误，不下载，也不加载嵌入模型。`NewOverlapReranker` 仍给原来的 `Retrieve` 使用。嵌入模型和重排模型不要同时加载。
