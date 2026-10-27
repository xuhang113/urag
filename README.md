# urag

## 测试

在仓库根目录执行：

```bash
go test ./...
```

只跑任务管道：

```bash
go test ./internal/job/
```

## 验收页

```bash
go run ./cmd/urag serve
```

浏览器打开 http://127.0.0.1:8080/upload 。其余页面是 `/fetch`、`/clean`、`/ask`、`/eval`。`GET /` 会跳到 `/upload`。
