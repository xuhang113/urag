# 2026-10-27 MySQL 台账

选题：用来源、父级章节和子块三张表记录清洗后的 UTF-8，原文不进对象存储。

## 做了什么

- `internal/catalog` 提供 `Store`。`doc_id` 和 `parent_id` 写入时生成，`chunk_id` 是文档编号、井号和序号。
- 同一集合的同一个 `source_uri` 只有一行。校验和变化时版本加 1，仍是同一个 `doc_id`。`ReplaceSections` 在一个事务里更换父块和子块。
- MySQL 实现使用调用方传入的 DSN。`NewMemory` 给测试用，测试不连接数据库，也不访问外网。

## 改了哪些路径

- `internal/catalog/`
- `specs/catalog.md`
- `go.mod`
- `go.sum`
- `changelog/2026-10-27-mysql-ledger.md`

## 怎么验收

```bash
go test ./internal/catalog/
```

应当通过。不需要启动 MySQL。

## 未做 / 下一步不要做

- 没有把台账接到 `job`、`web` 或 `cmd/urag`。
- 没有改其他包，也没有改召回夹具。
