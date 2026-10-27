# 收录

## 要做什么

把本地目录或 HTTP 地址收成原始对象。`Ingest` 把 `body` 流式交给 `storage.Store`。`IngestSource` 对每个条目调用同一条路径。每条幂等键是 `idemPrefix + ":" + sourceURI`。结果都是 `Result`：对象元数据、来源地址、集合编号、幂等键。

HTTP 来源的测试只使用 `httptest`。

有大小上限的读取器：任意一次 `Read` 的 `len(p)` 大于 `storage.CopyBufSize` 就返回错误。测试正文的长度必须大于这个上限。

## 不做什么

不使用 `io.ReadAll`。单次 `Read` 不得超过 `storage.CopyBufSize`。不清洗、不切分、不嵌入。不访问外网。`NewDatabase`、`NewSitemap`、`NewVendorAPI` 尚未接通，调用返回 `ErrNotImplemented`，它们不是可用实现。

## 涉及的包

`internal/acquire` 依赖 `internal/model` 与 `internal/storage`。

## 数据格式

来源种类：`local_dir`、`http`、`database`、`sitemap`、`vendor_api`。

`Item` 含来源地址、文件名、内容类型和 `Open`。`Open` 返回正文流，调用方关闭。

`Result.Meta` 是提交成功后的可见对象。集合与幂等键等于入参。校验和不符时返回 `storage.ErrChecksumMismatch`，不产生可见对象。

## 测试

### T2.1 TestLocalAndHTTPSameLineage

同一份正文分别从本地目录和 `httptest` 服务收入同一集合。断言两条 `Result` 都有非空对象键和校验和，集合编号等于入参，幂等键等于 `前缀:来源地址`。两条结果的形状一致：元数据、来源地址、集合、幂等键。测试期间没有对回环地址以外的拨号。

### T2.2 TestUploadBoundedReader

读取器在 `len(p) > storage.CopyBufSize` 时返回错误。正文长度大于该上限。`Ingest` 成功，校验和等于全文的小写十六进制 SHA-256。实现若一次请求更大的缓冲，测试失败。
