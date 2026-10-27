# 架构

urag 把外部材料收成可检索的集合。一条材料按固定顺序走完，不在 HTTP 处理函数里清洗或计算向量。

## 链路

1. 获取或上传。本地目录和 HTTP 地址走 `acquire`。浏览器上传走 `job.AcceptUpload`。两种入口都只把正文流式交给 `storage.Store`。
2. 原始对象。存储计算小写十六进制 SHA-256，校验和与大小一致后对象才可见。调用方这时已经拿到对象键、校验和、大小和任务编号。
3. 清洗文档。任务读取原始对象，得到 UTF-8 正文和血缘。集合内正文相同的后一篇标成重复，`Duplicate` 为 true，并指向第一篇。重复文档仍可查询，但不进入索引。
4. 片段。默认按段落切分。段落切分尚未接通时，短文整篇成为一个片段，编号是 `documentID#0`，字节区间落在文档正文上。
5. 向量。片段文本交给 `embed.Embedder`，再写入该集合自己的索引。评价夹具使用夹具里的文档编号，不用内容哈希替换。

提问只打开请求里点名的集合。评价先按题目的 `collections` 调用 `Ask`，再用 `eval.RecallAtK` 和 `eval.MeanReciprocalRank` 汇总。

## 集合隔离

一份文档只属于一个集合。一个集合一个索引。检索、清洗去重和评价打分都不跨到未点名的集合。夹具里的 gamma 只作为干扰文档，没有任何题目查询它。

## 任务状态

| 状态 | 含义 |
| --- | --- |
| queued | 已入队，尚未运行 |
| running | 正在运行 |
| succeeded | 成功结束 |
| failed | 失败，且不会再自动重试 |
| canceled | 已取消，之后不再重试 |

`URAG_INLINE_JOBS` 或 `job.Config.Inline` 为 true 时，清洗和建索引在 `AcceptUpload` 或 `SubmitFetch` 返回前做完，成功时状态是 succeeded。否则任务异步执行，返回时状态是 queued 或 running。

任务种类是 upload、fetch、eval。

## 断点与重试

失败最多尝试 3 次，次数含第一次。`context.Canceled` 和校验和不符不重试。`Cancel` 之后状态变为 canceled，正在进行的步骤被取消，循环不再进入下一次。

`Job.Checkpoint` 是 JSON，字段 `object_keys` 记下已经处理完的对象键。同一次任务再次执行时跳过这些键。抓取任务按条目推进，前面的对象已经记入断点后，后面的失败不会把前面的对象再清洗一遍。
