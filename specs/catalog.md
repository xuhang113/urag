# 台账

## 要做什么

用 MySQL 记录来源、父级章节和子块。一篇来源一行。`doc_id` 和 `parent_id` 在写入时由 `AUTO_INCREMENT` 生成。`chunk_id` 是字符串主键，格式为 `strconv.FormatInt(docID, 10) + "#" + ordinal`，序号用十进制、不补零。

`PutDocument` 忽略调用方传入的 `DocID`。同一 `collection_id` 和 `source_uri` 再次插入不产生第二行，返回已有 `doc_id`。校验和相同则整行保持不变。校验和不同则已保存的 `version` 加 1，仍是同一个 `doc_id`，并更新校验和、媒体类型、许可、标题和抓取时间。这次传入的 `Version` 不覆盖该规则。旧的父块和子块留着，由调用方决定要不要调用 `ReplaceSections`。

`ReplaceSections` 在一个事务里换掉该文档的全部父块和子块。新的 `parent_id` 由存储生成。子块的 `chunk_id`、`parent_id`、`doc_id` 由存储填写。批次里有重复序号时整次失败，旧段落保持原样。

`CountIndexed` 返回该集合的 `source_document` 行数。表里没有重复标记。调用方只写入清洗后不是重复、且至少有一个子块的文档。计数本身不检查子块。

`NewMemory` 提供同样的行为。`NewMySQL` 使用调用方传入的 DSN。`ApplySchema` 执行 `internal/catalog/schema.sql`。

## 不做什么

不把原文放进对象存储。不保存 PDF 或 DOCX 字节。不保存向量。没有 `object_key` 列。不在代码里读取环境变量，也不把口令写进仓库。`go test` 只用 `NewMemory`，不连接 MySQL，不访问外网。

## 涉及的包

`internal/catalog`。MySQL 实现使用 `database/sql` 和 `github.com/go-sql-driver/mysql`。不依赖其他 `internal` 包。

## 数据格式

表结构见 `internal/catalog/schema.sql`。

- `source_document`：`doc_id`，`collection_id`，`version`，`source_uri`，`checksum`，`media_type`，`license`，`title`，`fetched_at`。
- `parent_section`：`parent_id`，`doc_id`，`heading_path`，`page_from`，`page_to`，`body`。`body` 是这一节的 UTF-8。
- `child_chunk`：`chunk_id`，`parent_id`，`doc_id`，`ordinal`，`start_byte`，`end_byte`，`heading_path`，`page`，`body`。
- `checksum` 是清洗后 UTF-8 的小写十六进制 SHA-256，共 64 个字符。
- `collection_id` 最长 128 个字符，`source_uri` 最长 512 个字符。二者的唯一索引因此低于 InnoDB 的 3072 字节上限。MySQL 8 的默认行格式满足这个长度。
- `heading_path` 最长 65535 字节。`body` 最长 16777215 字节。
- `fetched_at` 以 UTC 保存，精度到微秒。零值存成 NULL。
- `start_byte` 与 `end_byte` 原样保存，台账不据此截取正文。
- 首次插入保存调用方传入的 `version`。

口令由调用方传入。变量名 `URAG_MYSQL_DSN` 只出现在注释和下面的示例里：

```text
example-user:fake-password@tcp(127.0.0.1:3306)/urag_example
```

## 测试

### T7.1 TestDuplicateSourceURIReturnsExistingDoc

同一集合、同一个 `source_uri` 写入两次。断言 `doc_id` 相同，表里只有一行，校验和相同时标题和版本不变。另一个集合的同一个 `source_uri` 是另一行。

### T7.2 TestGeneratedIDsAndChunkFormat

`doc_id` 和 `parent_id` 由存储生成，不采用调用方传入的编号。子块引用这两个编号。`chunk_id` 等于 `strconv.FormatInt(docID, 10) + "#" + ordinal`。调用方传入的 `chunk_id` 被忽略。

### T7.3 TestGetParentReturnsSectionBody

`GetParent` 取回标题路径、页码和整节 UTF-8 正文。

### T7.4 TestChecksumChangeKeepsDocAndBumpsVersion

校验和变化后仍是原来的 `doc_id`，`version` 比已保存的值大 1，不采用这次传入的版本号。旧子块还在。

### T7.5 TestReplaceSectionsSwapsInOneStep

重复序号使整次替换失败，旧节仍可取回。成功替换后旧父块消失，新子块引用新的 `parent_id`，`chunk_id` 格式正确。清空段落后来源行还在。

### T7.6 TestCountIndexedCountsSourceRows

`CountIndexed` 等于该集合的来源行数。没有子块的来源也计入。别的集合不计入。

### T7.7 TestSchemaHasNoObjectBytesOrVectors

`schema.sql` 只有这三张表。`doc_id` 和 `parent_id` 为 `AUTO_INCREMENT`。`chunk_id` 是主键，不是自动递增。语句里没有对象键、向量或二进制大对象。

### T7.8 TestMySQLDSNNameStaysInComments

`URAG_MYSQL_DSN` 和示例 DSN 只出现在注释和本文档，不出现在代码字符串里。

### T7.9 TestNewMySQLRejectsEmptyDSN

空 DSN 和无法解析的 DSN 返回错误。测试不打开连接。
