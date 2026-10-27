# 收录

## 要做什么

把本地目录或 HTTP 地址收成原始对象。`Ingest` 把 `body` 流式交给 `storage.Store`。`IngestSource` 对每个条目调用同一条路径。每条幂等键是 `idemPrefix + ":" + sourceURI`。结果都是 `Result`：对象元数据、来源地址、集合编号、幂等键。对象键是 `集合编号 + "/" + 幂等键`。键为空或含有 `..` 时返回 `ErrInvalidKey`，不调用 `Put`。

公开收录从站点地图或公开目录页列出不登录就能下载的 PDF、DOCX、HTML、TXT、Markdown。`Crawl` 只返回 `Found` 和 `Skip`，不把正文写入 `storage.Store`。同一 `SourceURI` 只出现一次，值是重定向之后的最终 URL。`Open` 在调用时再打开正文流。

`NewSitemap` 解析 urlset 和 sitemapindex，只把允许的文档 URL 收进 `List`。构造函数不拨号。它不再返回 `ErrNotImplemented`。

HTTP 来源和公开收录的测试只使用 `httptest`。

有大小上限的读取器：任意一次 `Read` 的 `len(p)` 大于 `storage.CopyBufSize` 就返回错误。测试正文的长度必须大于这个上限，并且长到 `io.ReadAll` 会发出大于该上限的 `Read`。

## 不做什么

不使用 `io.ReadAll`。单次 `Read` 不得超过 `storage.CopyBufSize`。不清洗、不切分、不嵌入。不访问外网。不带 Cookie 重试，也不绕过登录、付费或验证码。`NewDatabase`、`NewVendorAPI` 尚未接通，调用返回 `ErrNotImplemented`。断点不在本包落盘。

## 涉及的包

`internal/acquire` 依赖 `internal/model` 与 `internal/storage`。公开收录不调用 `Store.Put`。

## 数据格式

来源种类：`local_dir`、`http`、`database`、`sitemap`、`vendor_api`。

`Item` 含来源地址、文件名、内容类型和 `Open`。`Open` 返回正文流，调用方关闭。

`Result.Meta` 是提交成功后的可见对象。集合与幂等键等于入参。校验和不符时返回 `storage.ErrChecksumMismatch`，不产生可见对象。

`CrawlLimits.PerHostInterval` 是同一主机相邻两次请求的最小间隔。传 `0` 表示不额外等待，测试使用 `0`。对真实站点应传 `1s`。`MaxBytes` 为 `0` 时，单次响应最多读取 `32<<20` 字节。

`Skip.Reason` 只取 `robots`、`login`、`payment`、`captcha`、`unsupported`、`download`。

`Found.MediaType` 只取 `pdf`、`docx`、`html`、`txt`、`md`。`Title` 和 `License` 只在页面或响应头里能看到时填写。

`Checkpoint.Done` 是已经处理的 `SourceURI`。`WithDone` 收下这些地址后，`Crawl` 不再列出，也不再请求。

## 公开收录

抓取某个主机前先取该主机的 `/robots.txt`。只采用与 `User-Agent: urag-acquire` 匹配的那一组；没有具体匹配时用 `User-agent: *`。`Disallow` 的路径跳过，原因 `robots`。`Allow` 与 `Disallow` 同时匹配时，较长的规则优先；长度相同则允许。`Crawl-delay` 大于 `PerHostInterval` 时以 `Crawl-delay` 为准。没有 robots.txt，或取不到 robots.txt 时，只受 `PerHostInterval` 限制。robots.txt 返回 401 或 403 时，该主机的地址都记为 `robots`。

状态码 401 和 403 记为 `login`，402 记为 `payment`。HTML 可见文本里的 `captcha` 记为 `captcha`，`paywall` 记为 `payment`，`login` 记为 `login`。这些情况放入 `skips`，不返回错误。下载失败记为 `download`。后缀已能判断为不支持的类型时记为 `unsupported`，并且不请求该地址。

站点地图支持 urlset 和 sitemapindex。地图本身不是文档。其中的文档地址会再核对 robots、状态码和类型。公开目录页会抽出其中的 http、https 链接；目录页本身若是 HTML，也列入 `found`。从站点地图里发现的 HTML 不再继续展开链接。

`NewSitemap.List` 只读取 robots.txt 和站点地图，按 URL 后缀留下 pdf、docx、html、htm、txt、md、markdown。它不下载文档正文。同一地址只出现一次，robots 拒绝的地址不列入。正文由条目的 `Open` 在之后打开。`List` 对真实站点按 1 秒间隔访问地图文件。

`ctx` 取消时返回已经列出的 `found` 和 `skips`，以及 `ctx.Err()`。

## 测试

### T2.1 TestLocalAndHTTPSameLineage

同一段字节分别从本地目录和 `httptest` 收入同一集合。本地来源 `Kind` 为 `local_dir`，只列出文件、跳过目录，`SourceURI` 是文件路径，`Filename` 是基名。HTTP 来源 `Kind` 为 `http`，只接受 http 和 https；`List` 返回一条且不拨号，`Open` 是带 context 的 GET；`client` 为 nil 时用默认客户端。两条 `Result` 的校验和相同，且等于正文的小写十六进制 SHA-256；集合编号相同并等于入参；`Meta`、`SourceURI`、`CollectionID`、`IdempotencyKey` 都有值；两个来源的 `Kind` 不同。幂等键等于 `idemPrefix + ":" + SourceURI`。对象键等于 `集合编号 + "/" + 幂等键`。`NewSitemap` 的构造函数不拨号，`Kind` 为 `sitemap`。`NewDatabase`、`NewVendorAPI` 返回 `ErrNotImplemented`。测试期间没有对回环地址以外的拨号。

### T2.2 TestUploadBoundedReader

正文长于 `storage.CopyBufSize`，并且长到 `io.ReadAll` 会请求更大的缓冲。读取器在 `len(p) > storage.CopyBufSize` 时返回错误。假 `Store` 的 `Put` 用不超过 `CopyBufSize` 的缓冲做 `io.CopyBuffer`。`Ingest` 把原来的 reader 交给 `Put` 并成功，存入的字节与原文一致，校验和等于全文的小写十六进制 SHA-256。对象键含 `..` 时返回 `ErrInvalidKey`，且不调用 `Put`。

### T2.3 TestCrawlSitemapRobotsAuthAndDone

`httptest` 上的 sitemapindex 指向 urlset。结果含 pdf 和 html，html 的标题和 `License` 响应头被记下。同一 pdf 地址只出现一次。重定向后的 `SourceURI` 是最终 URL。`Disallow` 的 pdf 原因是 `robots` 且不请求；更长的 `Allow` 仍列入。另一组 User-agent 的 `Disallow: /` 和 `Crawl-delay: 10` 不生效。401 原因是 `login`，只请求一次，响应里的 `Set-Cookie` 不会出现在后续请求。403 也是 `login`，402 是 `payment`。HTML 可见文本中的 `captcha`、`paywall` 分别记为 `captcha`、`payment`。404 记为 `download`。png 记为 `unsupported` 且不请求。`WithDone` 里的地址不列入、不请求。`PerHostInterval` 为 0 时整次收录在数秒内完成。测试客户端拒绝回环以外的地址。收录阶段对 pdf 使用 HEAD，`Open` 才 GET 到字节。测试不构造 `storage.Store`。

### T2.4 TestCrawlDirectoryPage

公开目录页列出 docx、txt、md，并列入目录页自己的 html、标题和 meta license。png 原因是 `unsupported` 且不请求。`mailto` 不列入。没有 robots.txt 时仍能列出。收录阶段不 GET docx 正文。

### T2.5 TestCrawlDelayOverridesInterval

`PerHostInterval` 为 0 且 robots 的 `Crawl-delay` 为 0.2 秒时，同一主机相邻两次请求的间隔大于 0.2 秒的大部分，并且小于 1 秒。

### T2.6 TestCrawlCancelReturnsPartial

取消发生在第二份 pdf 的请求上时，已经列出的第一份 pdf 仍然返回，错误是 `context.Canceled`。

### T2.7 TestNewSitemapListsDocumentURLs

构造函数拒绝 ftp 和没有主机名的地址，且不拨号。`List` 解析 sitemapindex 和 urlset，pdf、html、txt 各出现一次，不下载这些文档。重复地址、png、robots 拒绝的 pdf 都不列入。`Open` 再读取 pdf 字节。测试把默认传输限制在回环地址。
