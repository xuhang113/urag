# 召回

评价读取 `URAG_FIXTURE`，默认 `testdata/recall/fixture.json`。`eval.Load` 尚未接通时，任务直接解析这份 JSON，不改 `eval` 包。

## 夹具

JSON 使用 snake_case。顶层字段：

- `k`：截断名次，夹具里是 5
- `thresholds`：`normal_recall_at_k`、`normal_mrr`、`contrast_recall_at_k_max`
- `contrast_drop_terms`：对照实验要整词删掉的词
- `collections`：每个集合有 `id` 和 `documents`。文档使用夹具里的 `id` 和 `text`
- `questions`：`id`、`collections`、`query`、`relevant_docs`

alpha 含 `alpha-lantern`、`alpha-canal`、`alpha-library`。beta 含 `beta-harbor`、`beta-market`。gamma 含 `gamma-secret`，只作干扰，没有任何题目查询它。

单集合题计入该集合。`collections` 多于一个的题只计入 `collection_id` 为 `merged` 的分数。建索引时文档编号原样使用，不改成内容哈希。

## Recall@K 和 MRR

`RecallAtK` 对每题先按文档编号去重，保留第一次出现，不按分数重排。相关文档至少一篇出现在前 K 名则该题记 1，否则记 0，再对题目做宏平均。

`MeanReciprocalRank` 取第一个相关文档名次的倒数，名次从 1 开始。没有命中记 0，再宏平均。

正常切分的名称是 `paragraph`。短文在段落切分尚未接通时整篇作为一个片段，编号 `documentID#0`。对照切分的名称是 `drop_keywords`：按 `contrast_drop_terms` 整词、不区分大小写删掉这些词，再把剩下的正文切成不超过 20 字节的碎片，然后用同一套题目和 `Ask` 打分。

## 阈值

数值从夹具读取，不在测试里另写更低的线：

- 正常：每个单集合的 Recall@K 不低于 0.8，MRR 不低于 0.6
- 对照：每个单集合的 Recall@K 不高于 0.4，并且比正常结果更差

低于普通阈值，或对照 Recall@K 高于 0.4，都不能当作召回合格。

## Direction

`eval.Report` 的字段是 `splitter`、`baseline`、`scores`、`recall_delta`、`mrr_delta`、`direction`。

`scores` 按集合分桶，每项有 `collection_id`、`recall_at_k`、`mrr`、`k`。多集合题在 `merged`。

- 正常结果没有对照基线可减，`direction` 为 `baseline`，两个 delta 为 0
- 对照结果相对正常结果的题目宏平均：变高为 `up`，变低为 `down`，相同为 `flat`
- `recall_delta` 和 `mrr_delta` 是对照的宏平均减去正常结果

没有这份对照报告，不能声称召回变好。只看到正常分数，或只改了切分而没有 `Direction`，都不算提升。
