# 存储

原文按对象键存放。读写都是流，单次复制缓冲不超过 `storage.CopyBufSize`（32 KiB）。本包不解析正文，也不出现云厂商客户端类型。

## Store

`storage.Store` 提供：

- `Put` / `Get` / `Delete` / `Stat`
- `ExistsByChecksum`
- `CreateMultipart` / `UploadPart` / `ListParts` / `CompleteMultipart` / `AbortMultipart`

`Put` 计算小写十六进制 SHA-256。`ExpectedChecksum` 为空时只计算；非空且与正文不符时返回 `ErrChecksumMismatch`，对象保持不可见，且不重试。`context.Canceled` 和 `context.DeadlineExceeded` 也不重试。其他可重试错误按 `RetryPolicy` 退避，`MaxAttempts` 含第一次调用。

密钥只来自调用方或环境变量，不写入日志和仓库。

## 幂等、临时键、分片续传

同一个非空幂等键的第二次 `Put` 返回第一次的 `ObjectMeta`，不产生第二份对象。任务层把幂等键交给存储，不自己另做一份去重。

写入先落在临时键上。校验和与大小都和正文一致后才提交元数据，对象这时才可见。提交失败时对象保持不可见。

分片上传在 `CompleteMultipart` 成功前不可见。中断后 `ListParts` 仍能看到已完成的分片，调用方只补缺失分片，再完成提交。

## 一个适配器覆盖多家对象存储

`storage.S3Config` 只有端点、区域、桶、路径风格、分片大小和退避。`NewS3` 按这份配置准备客户端。接口和配置里不出现某一家云的 SDK 类型。

同一套参数可以指向：

| 服务 | 接法 |
| --- | --- |
| MinIO | 自建 `Endpoint`，`PathStyle` 为 true |
| AWS S3 | 区域和桶；路径风格按需要打开 |
| 阿里云 OSS | S3 兼容端点，通常打开 `PathStyle` |
| 腾讯云 COS | S3 兼容端点，通常打开 `PathStyle` |

`URAG_STORAGE=s3` 时才读取 `URAG_S3_ENDPOINT`、`URAG_S3_REGION`、`URAG_S3_BUCKET`、`URAG_S3_ACCESS_KEY`、`URAG_S3_SECRET_KEY`、`URAG_S3_PATH_STYLE`。缺端点、桶或密钥时进程退出并提示，不伪造客户端。`PATH_STYLE` 为 `1`、`true`、`TRUE` 或 `True` 时打开路径风格。

本地目录使用 `URAG_STORAGE=local` 和 `URAG_DATA_DIR`，不走 S3。

## 再加一家厂商

能说 S3 兼容协议的厂商，换端点和路径风格即可，不必改获取、清洗或索引。

协议对不上时，新写一个实现 `Store` 的适配器，在 `cmd/urag` 里按配置选用。不要把厂商类型加进 `Store`，也不要改 `acquire`、`clean` 或 `index`。
