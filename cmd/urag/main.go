// Command urag 从环境变量组装依赖。
// serve 启动验收页。crawl 从来源 URL 收录，不把原文写入对象存储。
// 它不实现存储、清洗、切分、嵌入或检索。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xuhang113/urag/internal/acquire"
	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
	"github.com/xuhang113/urag/internal/web"
)

// qwenMeta 是本地嵌入权重的模型名和维度。目录里的配置必须与此一致，程序不改维度，也不下载。
var qwenMeta = embed.ModelMeta{Name: "Qwen/Qwen3-Embedding-4B", Dim: 2560}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error()))
		os.Exit(1)
	}
}

// redact 去掉错误文本里的 MySQL 账号口令和主机，避免收录日志写出完整 DSN。
func redact(msg string) string {
	return dsnPattern.ReplaceAllString(msg, "mysql-dsn-redacted")
}

var dsnPattern = regexp.MustCompile(`[^\s:@]+:[^@\s]+@tcp\([^)]*\)(?:/[^\s?]*)?(?:\?[^\s]*)?`)

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		if len(args) > 1 {
			return fmt.Errorf("urag: unexpected arguments")
		}
		return serve()
	case "crawl":
		return crawl(args[1:])
	case "index":
		return indexCollection(args[1:])
	case "search":
		return searchCollection(args[1:])
	case "try":
		return tryPage(args[1:])
	case "sync-disk":
		return syncDisk(args[1:])
	case "load-bm25":
		return loadBM25(args[1:])
	default:
		return fmt.Errorf("urag: unknown command %q", cmd)
	}
}

// crawl 从种子文件读取站点地图或目录页 URL。
// 没有 URAG_MYSQL_DSN 时退出。权重目录 URAG_EMBED_MODEL_DIR 不存在时退出，不下载。
func crawl(args []string) error {
	if len(args) != 1 {
		return errors.New("urag: crawl 需要一个种子文件")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	seeds, err := readSeeds(args[0])
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN"))
	if dsn == "" {
		return errors.New("urag: 缺少 URAG_MYSQL_DSN，无法写入台账")
	}
	modelDir := strings.TrimSpace(os.Getenv("URAG_EMBED_MODEL_DIR"))
	if modelDir == "" {
		return errors.New("urag: 缺少 URAG_EMBED_MODEL_DIR，不会下载嵌入模型")
	}
	info, err := os.Stat(modelDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("urag: 嵌入权重目录 %s 不存在，不会下载", modelDir)
	}

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	cfg.Inline = true
	cfg.CrawlCheckpoint = filepath.Join(cfg.DataDir, "crawl-checkpoint.json")
	db, err := catalog.NewMySQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.ApplySchema(ctx); err != nil {
		return err
	}
	emb, err := embed.NewQwen(modelDir, qwenMeta)
	if err != nil {
		return err
	}
	// 整次收录只启动一个嵌入进程。种子分批和失败重试都复用它。
	// 结束、失败或取消时关掉。重排仍用重叠重排，不加载重排模型。
	starter, ok := emb.(interface {
		Start(context.Context) error
		Close() error
	})
	if !ok {
		return errors.New("urag: 嵌入进程无法常驻")
	}
	if err := starter.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = starter.Close() }()
	reg, err := index.NewDisk(filepath.Join(cfg.DataDir, "vectors"), qwenMeta)
	if err != nil {
		return err
	}
	disk, ok := reg.(*index.Disk)
	if !ok {
		return errors.New("urag: 磁盘索引不可用")
	}
	// 收录不调用 Store.Put。本地目录只用来满足管道构造。
	store, err := storage.NewLocal(cfg.DataDir, cfg.Retry, nil)
	if err != nil {
		return err
	}
	pipeline, err := job.New(cfg, job.Deps{
		Store:    store,
		Embedder: emb,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Catalog:  db,
		Vectors:  disk,
	})
	if err != nil {
		return err
	}
	col := model.CollectionID(envOr("URAG_COLLECTION", "public"))
	// 每批列完就入库并写下断点。整份种子一起列完才入库时，后面的地址会挡住前面的篇数。
	// 原文不写入对象存储。嵌入进程在上面已经启动，这里不再加载模型。
	for _, batch := range splitSeedBatches(seeds, crawlSeedBatch) {
		result, err := pipeline.SubmitCrawl(ctx, job.CrawlInput{
			CollectionID: col,
			Seeds:        batch,
			Limits:       acquire.CrawlLimits{PerHostInterval: time.Second},
		})
		pipeline.ReleaseJob(result.ID)
		if err != nil {
			msg := result.Error
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("urag: crawl 停在种子 %s: %s", batch[0], redact(msg))
		}
		n, err := db.CountIndexed(ctx, string(col))
		if err != nil {
			return err
		}
		fmt.Printf("收录进行中，集合 %s 篇数 %d\n", col, n)
	}
	n, err := db.CountIndexed(ctx, string(col))
	if err != nil {
		return err
	}
	fmt.Printf("收录完成，集合 %s 篇数 %d\n", col, n)
	return nil
}

const crawlSeedBatch = 8

func splitSeedBatches(seeds []string, size int) [][]string {
	if size <= 0 {
		size = 1
	}
	var out [][]string
	for start := 0; start < len(seeds); start += size {
		end := start + size
		if end > len(seeds) {
			end = len(seeds)
		}
		out = append(out, seeds[start:end])
	}
	return out
}

func readSeeds(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("urag: 读取种子文件: %w", err)
	}
	defer f.Close()
	var seeds []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seeds = append(seeds, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("urag: 读取种子文件: %w", err)
	}
	if len(seeds) == 0 {
		return nil, errors.New("urag: 种子文件没有 URL")
	}
	return seeds, nil
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
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
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
	embedBatch, err := envInt("URAG_EMBED_BATCH_SIZE", job.DefaultEmbedBatch)
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
	// Qwen rerank 分数，低于此视为不相关。未设置时用正数默认门槛。
	minScore, err := envFloat("URAG_RERANK_MIN_SCORE", retrieve.DefaultRerankMinScore)
	if err != nil {
		return job.Config{}, err
	}
	if math.IsNaN(minScore) || math.IsInf(minScore, 0) {
		return job.Config{}, errors.New("urag: URAG_RERANK_MIN_SCORE 不是有限数")
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
		RerankMinScore: minScore,
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
	batch, err := envInt("URAG_EMBED_BATCH_SIZE", job.DefaultEmbedBatch)
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
