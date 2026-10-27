// Command urag 从环境变量组装依赖并启动验收页。
// 它不实现存储、清洗、切分、嵌入或检索。
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

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
	if len(args) > 1 {
		return fmt.Errorf("urag: unexpected arguments")
	}
	return serve()
}

// serve 是默认子命令。
// URAG_ADDR 默认 :8080。URAG_STORAGE 默认 local。URAG_DATA_DIR 默认 ./data。
// URAG_FIXTURE 默认 testdata/recall/fixture.json。
// 未设置 URAG_EMBED_ENDPOINT 时使用 64 维夹具嵌入。
// Storage 为 s3 且端点、桶或密钥不齐时返回错误，不伪造可用客户端。
func serve() error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	emb, err := openEmbedder()
	if err != nil {
		return err
	}
	pipeline, err := job.New(cfg, job.Deps{
		Store:    store,
		Embedder: emb,
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

func configFromEnv() (job.Config, error) {
	retryMax, err := envInt("URAG_RETRY_MAX", 3)
	if err != nil {
		return job.Config{}, err
	}
	vector, err := envFloat("URAG_VECTOR_WEIGHT", 0.7)
	if err != nil {
		return job.Config{}, err
	}
	keyword, err := envFloat("URAG_KEYWORD_WEIGHT", 0.3)
	if err != nil {
		return job.Config{}, err
	}
	maxLen, err := envInt("URAG_CHUNK_MAX_LEN", 800)
	if err != nil {
		return job.Config{}, err
	}
	overlap, err := envInt("URAG_CHUNK_OVERLAP", 100)
	if err != nil {
		return job.Config{}, err
	}
	cleanN, err := envInt("URAG_CLEAN_CONCURRENCY", 2)
	if err != nil {
		return job.Config{}, err
	}
	embedBatch, err := envInt("URAG_EMBED_BATCH_SIZE", 16)
	if err != nil {
		return job.Config{}, err
	}
	embedN, err := envInt("URAG_EMBED_CONCURRENCY", 2)
	if err != nil {
		return job.Config{}, err
	}
	uploadN, err := envInt("URAG_UPLOAD_CONCURRENCY", 2)
	if err != nil {
		return job.Config{}, err
	}
	inline, err := envBool("URAG_INLINE_JOBS")
	if err != nil {
		return job.Config{}, err
	}
	return job.Config{
		Addr:              envOr("URAG_ADDR", ":8080"),
		DataDir:           envOr("URAG_DATA_DIR", "./data"),
		Storage:           envOr("URAG_STORAGE", "local"),
		FixturePath:       envOr("URAG_FIXTURE", "testdata/recall/fixture.json"),
		Inline:            inline,
		UploadConcurrency: uploadN,
		Retry:             storage.RetryPolicy{MaxAttempts: retryMax},
		Weights:           retrieve.Weights{Vector: vector, Keyword: keyword},
		Chunk:             chunk.Options{MaxLen: maxLen, Overlap: overlap},
		Limits: job.Limits{
			Clean:            cleanN,
			EmbedBatch:       embedBatch,
			EmbedConcurrency: embedN,
		},
	}, nil
}

func openStore(cfg job.Config) (storage.Store, error) {
	switch cfg.Storage {
	case "s3":
		remote, err := storage.ConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		if remote.Endpoint == "" || remote.Bucket == "" || remote.AccessKey == "" || remote.SecretKey == "" {
			return nil, errors.New("urag: S3 配置不完整，需要 URAG_S3_ENDPOINT、URAG_S3_BUCKET、URAG_S3_ACCESS_KEY、URAG_S3_SECRET_KEY")
		}
		if v := os.Getenv("URAG_PART_SIZE"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("urag: URAG_PART_SIZE: %w", err)
			}
			remote.PartSize = n
		}
		remote.Retry = cfg.Retry
		return storage.NewS3(remote)
	case "local":
		return storage.NewLocal(cfg.DataDir, cfg.Retry, nil)
	default:
		return nil, fmt.Errorf("urag: unknown storage %q", cfg.Storage)
	}
}

func openEmbedder() (embed.Embedder, error) {
	endpoint := os.Getenv("URAG_EMBED_ENDPOINT")
	if endpoint == "" {
		return embed.NewFixture(64), nil
	}
	batch, err := envInt("URAG_EMBED_BATCH_SIZE", 16)
	if err != nil {
		return nil, err
	}
	concurrency, err := envInt("URAG_EMBED_CONCURRENCY", 2)
	if err != nil {
		return nil, err
	}
	return embed.NewHTTP(embed.Config{
		BatchSize:   batch,
		Concurrency: concurrency,
		Endpoint:    endpoint,
		Model:       os.Getenv("URAG_EMBED_MODEL"),
		APIKeyEnv:   "URAG_EMBED_API_KEY",
	}, http.DefaultClient.Do), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("urag: %s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("urag: %s: %w", key, err)
	}
	return n, nil
}

func envBool(key string) (bool, error) {
	switch os.Getenv(key) {
	case "", "0", "false", "FALSE", "False":
		return false, nil
	case "1", "true", "TRUE", "True":
		return true, nil
	default:
		return false, fmt.Errorf("urag: %s: want true or false", key)
	}
}
