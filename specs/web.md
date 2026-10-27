# 页面与接口

## 要做什么

用 `httptest` 点通五个页面和对应 API。`GET /` 跳转到 `/upload`。页面为 `GET /upload`、`/fetch`、`/clean`、`/ask`、`/eval`。

`POST /api/uploads` 接受表单字段 `collection`、`idempotency_key`、`file`。成功时 202，JSON 含 `job_id`、`object_key`、`checksum`、`size`、`collection_id`。

`POST /api/fetch` 接受 JSON：`collection_id`、`kind`、`uri`、`idempotency_prefix`。成功时 202，JSON 含 `job_id`。

`GET /api/jobs/{id}` 返回任务。`GET /api/collections/{id}/documents` 返回该集合文档。`POST /api/retrieve` 的 JSON 是 `retrieve.Request`。`POST /api/eval` 与 `GET /api/eval/report` 返回 `eval.Report`。

上传文件流式进入存储。调用方立刻拿到任务编号、对象键、校验和和大小。清洗和嵌入在任务里做。

## 不做什么

不在 `web` 里计算向量。不把上传正文读进内存。不搜索未点名的集合。不访问外网。页面测试使用 `httptest`，不监听真实端口。`web` 只依赖 `model`、`job`、`retrieve`、`eval`。

## 涉及的包

`internal/web` 调用 `internal/job`。提问使用 `internal/retrieve`。评价使用 `internal/eval`。命令 `cmd/urag` 只做组装，默认子命令是 `serve`，地址默认 `:8080`。

## 数据格式

上传响应字段：`job_id`、`object_key`、`checksum`、`size`、`collection_id`。`checksum` 是文件的小写十六进制 SHA-256。

抓取请求字段：`collection_id`、`kind`、`uri`、`idempotency_prefix`。

任务 JSON 与 `job.Job` 的标签一致，状态只取 `queued`、`running`、`succeeded`、`failed`、`canceled`，种类只取 `upload`、`fetch`、`eval`。

检索请求字段：`collections`、`query`、`top_k`、`hybrid`、`rerank`。命中含 `collection_id`、`document_id`、`chunk_id`、`text`、`score`、`start_byte`、`end_byte`。

评价报告含 `splitter`、`baseline`、`scores`、`recall_delta`、`mrr_delta`、`direction`。每个出现过题目的集合都有 Recall@K 和 MRR。多集合题在 `collection_id` 为 `merged` 的项里。

## 测试

五个测试都使用 `httptest`。先请求页面，再请求该页面对应的 API。

### T6.1 TestPageUpload

`GET /upload` 返回 200，正文能辨认上传页。`POST /api/uploads` 返回 202。JSON 含非空 `object_key`、`checksum`、`job_id`，并含 `size` 与 `collection_id`。`checksum` 等于所传文件的小写十六进制 SHA-256。

### T6.2 TestPageFetch

`GET /fetch` 返回 200，正文能辨认抓取页。`POST /api/fetch` 返回 202，JSON 含非空 `job_id`。

### T6.3 TestPageClean

`GET /clean` 返回 200，正文能辨认清洗页。同一集合提交两篇相同正文后，清洗页或 `GET /api/collections/{id}/documents` 能看到重复文档：`Duplicate` 为 true，且 `DuplicateOf` 指向第一篇。

### T6.4 TestPageAsk

`GET /ask` 返回 200，正文能辨认提问页。`POST /api/retrieve` 的命中含片段正文、分数、集合编号，以及 `start_byte` 和 `end_byte`。这两个下标能在原文上取回该片段。

### T6.5 TestPageEval

`GET /eval` 返回 200，正文能辨认评价页。`POST /api/eval` 与 `GET /api/eval/report` 的报告里，每个有题目的集合都有 `recall_at_k` 和 `mrr`。alpha、beta 和 merged 都出现。
