# 存储

## 要做什么

提供按对象键存放原文的契约。写入和读取都是流。复制缓冲不超过 `storage.CopyBufSize`（32 KiB）。写入时计算小写十六进制 SHA-256。校验和与大小一致后才提交元数据。先写临时键，提交成功后对象才可见。

同一集合内，同一个非空幂等键的第二次写入返回第一次的 `ObjectMeta`，不产生第二份对象。可重试错误按 `RetryPolicy` 做有限次退避。`MaxAttempts` 包含第一次调用。小于 1 时按 3 次。`BaseDelay` 为 0 时不休眠。

分片上传中断后，`ListParts` 仍返回已完成分片。调用方只补缺失分片，再 `CompleteMultipart`。

`ConfigFromEnv` 只从下列变量组装 `S3Config`，不创建客户端：`URAG_S3_ENDPOINT`、`URAG_S3_REGION`、`URAG_S3_BUCKET`、`URAG_S3_ACCESS_KEY`、`URAG_S3_SECRET_KEY`、`URAG_S3_PATH_STYLE`。`PATH_STYLE` 为 `1`、`true`、`TRUE` 或 `True` 时 `PathStyle` 为 true。`PartSize` 与 `Retry` 保持零值。`getenv` 为 nil 时返回错误。

## 不做什么

不解析、清洗或索引正文。不把整个对象读进内存。不重试 `ErrChecksumMismatch`、`context.Canceled`、`context.DeadlineExceeded`。校验和不符时对象保持不可见。导出 API 不出现云厂商客户端类型。不把密钥写入日志或仓库。

## 涉及的包

`internal/storage` 只依赖 `internal/model`。

## 数据格式

- `Checksum`：小写十六进制 SHA-256。
- `ObjectMeta`：键、大小、校验和、内容类型、集合编号、幂等键。
- `PutOptions.ExpectedChecksum` 为空时只计算；非空且不符时返回 `ErrChecksumMismatch`。
- `MultipartSession` 在提交前不可见。`Part` 含分片号、大小、校验和、ETag。
- `Hook.OnCommit` 的 attempt 从 1 开始。返回错误则对象不可见。hook 为空时照常提交。
- `Local.CommitAttempts` 等于已经进入提交步骤的次数。校验和不符、上下文取消和幂等命中不进入提交。`NewLocal` 在目录上提供上述保证。`NewS3` 缺少端点、区域、桶或密钥时返回错误，不创建客户端。

## 测试

### T1.1 TestLocalStreamPutGet

用长度大于 `CopyBufSize` 的正文调用 `Put`，再 `Get`。断言字节一致，`Size` 等于字节数，`Checksum` 等于该正文的小写十六进制 SHA-256。读取器在 `len(p) > CopyBufSize` 时返回错误；测试能通过，说明实现没有一次读取更大的缓冲。

### T1.2 TestChecksumMismatchNotRetried

`ExpectedChecksum` 与正文不符。断言返回 `ErrChecksumMismatch`，`CommitAttempts` 仍为 0，`Get` 或 `Stat` 返回 `ErrNotFound`。

### T1.3 TestIdempotentPut

同一非空幂等键写入两次。断言两次 `ObjectMeta` 相同，存储里只有一份对象。

### T1.4 TestMultipartResume

上传部分分片后中断。断言 `ListParts` 只含已完成分片。只补缺失分片并 `CompleteMultipart` 后，对象可见，校验和与大小正确。

### T1.5 TestPutCancel

`Put` 期间取消上下文。断言返回 `context.Canceled`，不重试，对象不可见。

### T1.6 TestExportedAPIHasNoVendorTypes

扫描 `storage` 包的导出标识符、字段类型和嵌入类型。类型字符串不得包含 `aws`、`minio`、`aliyun`、`tencent`。
