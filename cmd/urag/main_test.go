package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/retrieve"
)

func TestReadSeedsSkipsComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seeds.txt")
	body := "\n# comment\n http://127.0.0.1/a.xml \n\nhttp://127.0.0.1/b.html\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSeeds(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "http://127.0.0.1/a.xml" || got[1] != "http://127.0.0.1/b.html" {
		t.Fatalf("%q", got)
	}
}

func TestSplitSeedBatches(t *testing.T) {
	got := splitSeedBatches([]string{"a", "b", "c", "d", "e"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 || got[2][0] != "e" {
		t.Fatalf("%v", got)
	}
}

func TestRedactDSN(t *testing.T) {
	got := redact("dial example-user:fake-password@tcp(127.0.0.1:3306)/urag_example?parseTime=true: denied")
	if strings.Contains(got, "fake-password") || strings.Contains(got, "127.0.0.1") || strings.Contains(got, "urag_example") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(got, "mysql-dsn-redacted") {
		t.Fatalf("%s", got)
	}
}

func TestCrawlExitsWithoutDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seeds.txt")
	if err := os.WriteFile(path, []byte("http://127.0.0.1/sitemap.xml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URAG_MYSQL_DSN", "")
	t.Setenv("URAG_EMBED_MODEL_DIR", t.TempDir())
	err := run([]string{"crawl", path})
	if err == nil || !strings.Contains(err.Error(), "URAG_MYSQL_DSN") {
		t.Fatalf("%v", err)
	}
}

func TestEmbedBatchDefaultIs64(t *testing.T) {
	for _, key := range []string{
		"URAG_RETRY_MAX",
		"URAG_VECTOR_WEIGHT",
		"URAG_KEYWORD_WEIGHT",
		"URAG_CHUNK_MAX_LEN",
		"URAG_CHUNK_OVERLAP",
		"URAG_CLEAN_CONCURRENCY",
		"URAG_EMBED_BATCH_SIZE",
		"URAG_EMBED_CONCURRENCY",
		"URAG_UPLOAD_CONCURRENCY",
		"URAG_INLINE_JOBS",
	} {
		t.Setenv(key, "")
	}
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.EmbedBatch != 64 {
		t.Fatalf("batch %d", cfg.Limits.EmbedBatch)
	}
	t.Setenv("URAG_EMBED_BATCH_SIZE", "32")
	cfg, err = configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.EmbedBatch != 32 {
		t.Fatalf("batch %d", cfg.Limits.EmbedBatch)
	}
}

func TestRerankMinScoreFromEnv(t *testing.T) {
	t.Setenv("URAG_RERANK_MIN_SCORE", "")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RerankMinScore != retrieve.DefaultRerankMinScore || cfg.RerankMinScore <= 0 {
		t.Fatalf("default %v", cfg.RerankMinScore)
	}
	t.Setenv("URAG_RERANK_MIN_SCORE", "0.2")
	cfg, err = configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RerankMinScore != 0.2 {
		t.Fatalf("%v", cfg.RerankMinScore)
	}
	t.Setenv("URAG_RERANK_MIN_SCORE", "nope")
	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "URAG_RERANK_MIN_SCORE") {
		t.Fatal(err)
	}
	t.Setenv("URAG_RERANK_MIN_SCORE", "NaN")
	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "有限数") {
		t.Fatal(err)
	}
}

func TestSearchFiltersByRerankScore(t *testing.T) {
	body, err := os.ReadFile("milvus_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "func searchCollection")
	end := strings.Index(text, "func rerankDir")
	if start < 0 || end < start {
		t.Fatal("searchCollection missing")
	}
	fn := text[start:end]
	if !strings.Contains(fn, "KeepAboveRerankScore") || !strings.Contains(fn, "RerankMinScore") {
		t.Fatal("search does not apply the rerank score gate")
	}
}

func TestCrawlKeepsOneEmbedProcess(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{"Start(ctx)", "starter.Close()", "NewOverlapReranker"} {
		if !strings.Contains(text, frag) {
			t.Fatalf("missing %s", frag)
		}
	}
	for _, frag := range []string{"NewQwenReranker", "storage.Put", ".Put("} {
		if strings.Contains(text, frag) {
			t.Fatalf("contains %s", frag)
		}
	}
}

func TestSearchAndIndexExitWithoutMilvusAddr(t *testing.T) {
	t.Setenv("URAG_MILVUS_ADDR", "")
	err := run([]string{"search", "crimson lantern"})
	if err == nil || !strings.Contains(err.Error(), "URAG_MILVUS_ADDR") || !strings.Contains(err.Error(), "不会连接") {
		t.Fatalf("%v", err)
	}
	err = run([]string{"index"})
	if err == nil || !strings.Contains(err.Error(), "URAG_MILVUS_ADDR") || !strings.Contains(err.Error(), "不会连接") {
		t.Fatalf("%v", err)
	}
}

func TestRerankDirFromEmbedCache(t *testing.T) {
	root := t.TempDir()
	embedDir := filepath.Join(root, "models--Qwen--Qwen3-Embedding-4B", "snapshots", "abc")
	rerank := filepath.Join(root, "models--Qwen--Qwen3-Reranker-4B")
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := rerankDir(embedDir); err == nil || !strings.Contains(err.Error(), "不会下载") {
		t.Fatalf("%v", err)
	}
	if err := os.MkdirAll(rerank, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := rerankDir(embedDir)
	if err != nil || got != rerank {
		t.Fatalf("%s %v", got, err)
	}
}

func TestReleaseEmbedderClosesBeforeReturn(t *testing.T) {
	var order []string
	emb := &orderEmbedder{mark: func() { order = append(order, "embed") }}
	rel := &releaseEmbedder{inner: emb, release: func() error {
		order = append(order, "close")
		return nil
	}}
	if _, err := rel.Embed(context.Background(), []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "embed,close" {
		t.Fatalf("%v", order)
	}
}

func TestIndexCommandDoesNotLoadReranker(t *testing.T) {
	body, err := os.ReadFile("milvus_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "func indexCollection")
	end := strings.Index(text, "func searchCollection")
	if start < 0 || end < start {
		t.Fatal("functions missing")
	}
	indexFn := text[start:end]
	if strings.Contains(indexFn, "NewQwenReranker") || strings.Contains(indexFn, "QueryText") {
		t.Fatal(indexFn)
	}
	for _, frag := range []string{"19530", "localhost", "@tcp("} {
		if strings.Contains(text, frag) {
			t.Fatalf("contains %s", frag)
		}
	}
}

type orderEmbedder struct {
	mark func()
}

func (o *orderEmbedder) Dim() int { return 1 }

func (o *orderEmbedder) Embed(context.Context, []string) ([]embed.Vector, error) {
	if o.mark != nil {
		o.mark()
	}
	return []embed.Vector{{1}}, nil
}

func TestTryDoesNotCreateVectors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URAG_DATA_DIR", dir)
	t.Setenv("URAG_MYSQL_DSN", "")
	t.Setenv("URAG_EMBED_MODEL_DIR", "")
	t.Setenv("URAG_RERANK_MODEL_DIR", "")
	t.Setenv("URAG_MILVUS_ADDR", "")
	err := run([]string{"try", "extra"})
	if err == nil || !strings.Contains(err.Error(), "不接收参数") {
		t.Fatal(err)
	}
	err = run([]string{"try"})
	if err == nil || !strings.Contains(err.Error(), "不会新建") {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "vectors")); !os.IsNotExist(statErr) {
		t.Fatalf("created vectors: %v", statErr)
	}
}

func TestParseVMStatFreePages(t *testing.T) {
	text := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                                6047.\nPages inactive:                          10.\n"
	pages, ok := parseVMStatFreePages(text)
	if !ok || pages != 6047 {
		t.Fatalf("pages %d ok %v", pages, ok)
	}
	if _, ok := parseVMStatFreePages("no free line"); ok {
		t.Fatal("missing free line was accepted")
	}
}

func TestTryCommandDoesNotWriteDSN(t *testing.T) {
	body, err := os.ReadFile("try_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{"@tcp(", "19530", "milvus.example", "password"} {
		if strings.Contains(text, frag) {
			t.Fatalf("contains %s", frag)
		}
	}
	if !strings.Contains(text, "URAG_MYSQL_DSN") || !strings.Contains(text, "NewQwenReranker") {
		t.Fatal("try command missing ledger or rerank hook")
	}
	if strings.Contains(text, "KeepEmbed") || strings.Contains(text, "starter.Start") || strings.Contains(text, "ParkEmbed") {
		t.Fatal("try still keeps the embedder resident and skips rerank")
	}
	if !strings.Contains(text, "Passages:") {
		t.Fatal("try does not pass the reranker into the pipeline")
	}
}

func TestCrawlExitsWhenModelDirMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seeds.txt")
	if err := os.WriteFile(path, []byte("http://127.0.0.1/sitemap.xml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URAG_MYSQL_DSN", "example-user:fake-password@tcp(127.0.0.1:3306)/urag_example")
	t.Setenv("URAG_EMBED_MODEL_DIR", filepath.Join(t.TempDir(), "no-such-weights"))
	err := run([]string{"crawl", path})
	if err == nil || !strings.Contains(err.Error(), "不会下载") {
		t.Fatalf("%v", err)
	}
}
