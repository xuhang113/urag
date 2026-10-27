package embed_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
)

func TestQueryText(t *testing.T) {
	const question = "crimson lantern"
	got := embed.QueryText(question)
	want := "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery:" + question
	if got != want || got != embed.QueryInstruct+question {
		t.Fatalf("QueryText = %q", got)
	}
	if strings.Contains(embed.QueryInstruct, "Query: ") || !strings.HasSuffix(embed.QueryInstruct, "Query:") {
		t.Fatalf("prefix = %q", embed.QueryInstruct)
	}
	if got[len(embed.QueryInstruct)] != question[0] {
		t.Fatal("space inserted after Query:")
	}
}

func TestNewQwenMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-weights")
	_, err := embed.NewQwen(dir, embed.ModelMeta{
		Name: "Qwen/Qwen3-Embedding-4B",
		Dim:  2560,
	})
	if err == nil {
		t.Fatal("missing weights directory should fail")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("missing directory was created")
	}
}

func TestNewQwenRejectsDimension(t *testing.T) {
	_, err := embed.NewQwen(t.TempDir(), embed.ModelMeta{
		Name: "Qwen/Qwen3-Embedding-4B",
		Dim:  768,
	})
	if err == nil {
		t.Fatal("dimension other than 2560 should fail")
	}
}
