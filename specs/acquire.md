# 收录

## 要做什么

把本地目录或 HTTP 地址收成原始对象。`Ingest` 把 `body` 流式交给 `storage.Store`。`IngestSource` 对每个条目调用同一条路径。每条幂等键是 `idemPrefix + ":" + sourceURI`。结果都是 `Result`：对象元数据、来源地址、集合编号、幂等键。对象键是 `集合编号 + "/" + 幂等键`。键为空或含有 `..` 时返回 `ErrInvalidKey`，不调用 `Put`。

HTTP 来源的测试只使用 `httptest`。

有大小上限的读取器：任意一次 `Read` 的 `len(p)` 大于 `storage.CopyBufSize` 就返回错误。测试正文的长度必须大于这个上限，并且长到 `io.ReadAll` 会发出大于该上限的 `Read`。

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

同一段字节分别从本地目录和 `httptest` 收入同一集合。本地来源 `Kind` 为 `local_dir`，只列出文件、跳过目录，`SourceURI` 是文件路径，`Filename` 是基名。HTTP 来源 `Kind` 为 `http`，只接受 http 和 https；`List` 返回一条且不拨号，`Open` 是带 context 的 GET；`client` 为 nil 时用默认客户端。两条 `Result` 的校验和相同，且等于正文的小写十六进制 SHA-256；集合编号相同并等于入参；`Meta`、`SourceURI`、`CollectionID`、`IdempotencyKey` 都有值；两个来源的 `Kind` 不同。幂等键等于 `idemPrefix + ":" + SourceURI`。对象键等于 `集合编号 + "/" + 幂等键`。`NewDatabase`、`NewSitemap`、`NewVendorAPI` 返回 `ErrNotImplemented`。测试期间没有对回环地址以外的拨号。

### T2.2 TestUploadBoundedReader

正文长于 `storage.CopyBufSize`，并且长到 `io.ReadAll` 会请求更大的缓冲。读取器在 `len(p) > storage.CopyBufSize` 时返回错误。假 `Store` 的 `Put` 用不超过 `CopyBufSize` 的缓冲做 `io.CopyBuffer`。`Ingest` 把原来的 reader 交给 `Put` 并成功，存入的字节与原文一致，校验和等于全文的小写十六进制 SHA-256。对象键含 `..` 时返回 `ErrInvalidKey`，且不调用 `Put`。
