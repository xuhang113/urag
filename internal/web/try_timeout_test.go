package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
)

func TestTryTimesOutWhenEmbedBlocks(t *testing.T) {
	setTryTimeout(t, 150*time.Millisecond)
	emb := &blockEmbedder{dim: 8, done: make(chan struct{})}
	h := tryHandler(t, emb, fixedPassages{score: 0.9})
	start := time.Now()
	rec := postTryQuery(t, h, "crimson lantern")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("try hung for %s", time.Since(start))
	}
	assertTryTimeout(t, rec)
	waitDone(t, emb.done)
}

func TestTryTimesOutWhenRerankBlocks(t *testing.T) {
	setTryTimeout(t, 150*time.Millisecond)
	rr := &blockPassages{done: make(chan struct{})}
	h := tryHandler(t, embed.NewFixture(8), rr)
	start := time.Now()
	rec := postTryQuery(t, h, "crimson lantern")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("try hung for %s", time.Since(start))
	}
	assertTryTimeout(t, rec)
	waitDone(t, rr.done)
}

func TestTryFastQueryIsNotTimedOut(t *testing.T) {
	setTryTimeout(t, 2*time.Second)
	h := tryHandler(t, embed.NewFixture(8), fixedPassages{score: 0.9})
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/try", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), tryTimeoutText) {
		t.Fatalf("page %d %s", page.Code, page.Body.String())
	}
	rec := postTryQuery(t, h, "crimson lantern")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var hits []retrieve.Hit
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits %+v", hits)
	}
}

func setTryTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := trySearchTimeout
	trySearchTimeout = d
	t.Cleanup(func() { trySearchTimeout = prev })
}

func tryHandler(t *testing.T, emb embed.Embedder, passages retrieve.PassageReranker) http.Handler {
	t.Helper()
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		DataDir: t.TempDir(),
		Weights: retrieve.Weights{Vector: 0.7, Keyword: 0.3},
		Chunk:   chunk.Options{MaxLen: 800, Overlap: 100},
	}, job.Deps{
		Store:    store,
		Embedder: emb,
		Registry: index.NewMemory(),
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: passages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Handler(p)
}

func postTryQuery(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/try", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertTryTimeout(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != tryTimeoutText {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("search step still running after the request ended")
	}
}

type blockEmbedder struct {
	dim  int
	done chan struct{}
}

func (b *blockEmbedder) Dim() int { return b.dim }

func (b *blockEmbedder) Embed(ctx context.Context, _ []string) ([]embed.Vector, error) {
	defer close(b.done)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("embed ignored the deadline")
	}
}

type blockPassages struct {
	done chan struct{}
}

func (b *blockPassages) Score(ctx context.Context, _ string, _ []string) ([]float64, error) {
	defer close(b.done)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("rerank ignored the deadline")
	}
}

type fixedPassages struct {
	score float64
}

func (f fixedPassages) Score(ctx context.Context, _ string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]float64, len(passages))
	for i := range out {
		out[i] = f.score
	}
	return out, nil
}
