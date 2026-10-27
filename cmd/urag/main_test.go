package main

import (
	"os"
	"path/filepath"
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
