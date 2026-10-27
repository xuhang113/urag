package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
