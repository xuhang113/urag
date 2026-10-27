# 2026-10-27 任务、验收页和文档

选题：把上传、抓取、清洗、提问和召回评价收成任务与五个验收页，并写中文文档。

## 做了什么

- `internal/job` 接收上传和抓取，在任务里清洗、切分、嵌入并写入集合索引。失败最多尝试 3 次；取消和校验和不符不重试。断点记下已处理的对象键。
- `internal/web` 提供五个服务端页面和对应 JSON。`cmd/urag serve` 从环境变量组装依赖。
- 补了架构、模块、存储、召回和运行文档，以及 `AGENTS.md`、`README.md`、`.env.example`。

## 改了哪些路径

- `internal/job/`
- `internal/web/`
- `cmd/urag/main.go`
- `docs/`
- `AGENTS.md`
- `README.md`
- `.env.example`
- `changelog/2026-10-27-urag-initial.md`

## 怎么验收

```bash
go test ./internal/job/
go test ./internal/web/
```

`go test ./internal/job/` 使用测试里的假存储、嵌入、索引和清洗，应当通过。五个页面测试走真实的存储、嵌入和索引构造函数。这些包在本分支仍返回未实现，所以页面测试失败，失败原因应当是 `not implemented`。不要为了让它们变绿而放宽断言，也不要在 web 里另写一套存储或索引。

## 未做 / 下一步不要做

- 与存储、获取、清洗、切分、嵌入、索引、检索、评价各模块合并后，整库 `go test ./...` 变绿。本分支不改那些包的生产代码。
