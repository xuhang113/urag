// Command urag 组装配置和桩并启动服务。
// 它不实现存储、清洗、切分、嵌入或检索。
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
	"github.com/xuhang113/urag/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd != "serve" {
		return fmt.Errorf("urag: unknown command %q", cmd)
	}
	return serve()
}

// serve 是默认子命令。
// URAG_ADDR 默认 :8080。URAG_STORAGE 默认 local。URAG_DATA_DIR 是本地数据目录。
// URAG_FIXTURE_PATH 默认 testdata/recall/fixture.json。
// Storage 为 s3 且端点、桶或密钥不齐时返回错误，不伪造可用客户端。
// 本地存储尚未接通时仍调用 web.Listen。Listen 失败时由 main 写到标准错误。
func serve() error {
	cfg := job.Config{
		Addr:        envOr("URAG_ADDR", ":8080"),
		DataDir:     os.Getenv("URAG_DATA_DIR"),
		Storage:     envOr("URAG_STORAGE", "local"),
		FixturePath: envOr("URAG_FIXTURE_PATH", "testdata/recall/fixture.json"),
		Chunk:       chunk.Options{MaxLen: 800, Overlap: 100},
		Weights:     retrieve.Weights{Vector: 0.7, Keyword: 0.3},
	}
	store, err := openStore(cfg)
	if err != nil && (cfg.Storage == "s3" || !errors.Is(err, storage.ErrNotImplemented)) {
		return err
	}
	pipeline, err := job.New(cfg, job.Deps{
		Store:    store,
		Embedder: embed.NewFixture(0),
		Registry: index.NewMemory(),
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(nil),
	})
	if err != nil {
		return err
	}
	return web.Listen(cfg.Addr, pipeline)
}

func openStore(cfg job.Config) (storage.Store, error) {
	switch cfg.Storage {
	case "s3":
		remote, err := storage.ConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		if remote.Endpoint == "" || remote.Bucket == "" || remote.AccessKey == "" || remote.SecretKey == "" {
			return nil, errors.New("urag: s3 config is incomplete")
		}
		return storage.NewS3(remote)
	case "local":
		return storage.NewLocal(cfg.DataDir, cfg.Retry, nil)
	default:
		return nil, fmt.Errorf("urag: unknown storage %q", cfg.Storage)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
