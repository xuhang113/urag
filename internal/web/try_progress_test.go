package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

func TestTryStreamKeepsLaterStagesUnfinished(t *testing.T) {
	emb := &phaseEmbedder{dim: 8, phase: make(chan string), next: make(chan struct{})}
	h := publicTryHandler(t, emb, &phaseReranker{emb: emb, score: 0.9}, retrieve.DefaultRerankMinScore)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	body := &lockedBuffer{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- readTryStream(ctx, srv.URL+"/api/try", "crimson lantern", body)
	}()

	waitPhase(t, ctx, emb.phase, "entered")
	waitText(t, ctx, body, `"name":"加载嵌入模型","done":false`)
	if strings.Contains(body.String(), `"done":true`) {
		t.Fatalf("a step was marked done before the embedder ran\n%s", body.String())
	}

	emb.next <- struct{}{}
	waitPhase(t, ctx, emb.phase, "loaded")
	waitText(t, ctx, body, `"name":"计算查询向量","done":false`)
	mid := body.String()
	if strings.Contains(mid, "向量检索") || strings.Contains(mid, "重排") {
		t.Fatalf("later stage started while the query vector was still running\n%s", mid)
	}
	if !strings.Contains(mid, `"name":"加载嵌入模型","done":true`) {
		t.Fatalf("embed load was not finished when the model reported it\n%s", mid)
	}

	emb.next <- struct{}{}
	waitPhase(t, ctx, emb.phase, "embedded")
	emb.next <- struct{}{}

	waitText(t, ctx, body, "event: result")
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertStepOrder(t, body.String(), []string{
		retrieve.StepLoadEmbed,
		retrieve.StepQueryVector,
		retrieve.StepUnloadEmbed,
		retrieve.StepVectorSearch,
		retrieve.StepBM25,
		retrieve.StepMergeTop50,
		retrieve.StepLoadRerank,
		retrieve.StepRerank,
		retrieve.StepFilter(retrieve.DefaultRerankMinScore),
		retrieve.StepParents,
	})
	if !strings.Contains(body.String(), "父节正文") || !strings.Contains(body.String(), `"collection_id":"public"`) {
		t.Fatalf("result %s", body.String())
	}
}

func TestTryStreamTimeoutLeavesStepOpen(t *testing.T) {
	setTryTimeout(t, 300*time.Millisecond)
	emb := &blockEmbedder{dim: 8, done: make(chan struct{})}
	h := tryHandler(t, emb, fixedPassages{score: 0.9})
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := &lockedBuffer{}
	if err := readTryStream(ctx, srv.URL+"/api/try", "crimson lantern", body); err != nil {
		t.Fatal(err)
	}
	text := body.String()
	if !strings.Contains(text, tryTimeoutText) {
		t.Fatalf("body %s", text)
	}
	if strings.Contains(text, `"done":true`) {
		t.Fatalf("timed out search marked a step done\n%s", text)
	}
	if !strings.Contains(text, `"name":"加载嵌入模型","done":false`) {
		t.Fatalf("load step missing\n%s", text)
	}
	waitDone(t, emb.done)
}

func publicTryHandler(t *testing.T, emb embed.Embedder, passages retrieve.PassageReranker, min float64) http.Handler {
	t.Helper()
	ctx := context.Background()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	reg, err := index.NewDisk(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	disk := reg.(*index.Disk)
	qvec := mustEmbed(t, embed.NewFixture(meta.Dim), embed.QueryText("crimson lantern"))
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{{
		ChunkID: "c1", ParentID: 42, DocID: 7, CollectionID: "public",
		HeadingPath: "东桥", Body: "crimson lantern east bridge",
		StartByte: 3, EndByte: 9, Vector: qvec,
	}}); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		DataDir:        t.TempDir(),
		Weights:        retrieve.Weights{Vector: 0.7, Keyword: 0.3},
		Chunk:          chunk.Options{MaxLen: 800, Overlap: 100},
		RerankMinScore: min,
	}, job.Deps{
		Store:    store,
		Embedder: emb,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: passages,
		Parents:  parentBody{42: "父节正文"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Handler(p)
}

func readTryStream(ctx context.Context, url, query string, dst *lockedBuffer) error {
	raw, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return errors.New(res.Status + " " + string(b))
	}
	buf := make([]byte, 1024)
	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func waitPhase(t *testing.T, ctx context.Context, phase <-chan string, want string) {
	t.Helper()
	select {
	case got := <-phase:
		if got != want {
			t.Fatalf("phase %s, want %s", got, want)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitText(t *testing.T, ctx context.Context, body *lockedBuffer, frag string) {
	t.Helper()
	for {
		if strings.Contains(body.String(), frag) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("missing %s\n%s", frag, body.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertStepOrder(t *testing.T, body string, want []string) {
	t.Helper()
	var finished []string
	state := map[string]bool{}
	for _, part := range strings.Split(body, "\n\n") {
		if !strings.Contains(part, "event: step") {
			continue
		}
		var data string
		for _, line := range strings.Split(part, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		var step struct {
			Name string `json:"name"`
			Done bool   `json:"done"`
		}
		if err := json.Unmarshal([]byte(data), &step); err != nil {
			t.Fatal(err)
		}
		if prev, ok := state[step.Name]; ok && prev && !step.Done {
			t.Fatalf("step %s went back to unfinished", step.Name)
		}
		if step.Done && !state[step.Name] {
			finished = append(finished, step.Name)
		}
		state[step.Name] = step.Done
	}
	if strings.Join(finished, ",") != strings.Join(want, ",") {
		t.Fatalf("finished\n%s\nwant\n%s\nbody\n%s", strings.Join(finished, ","), strings.Join(want, ","), body)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type phaseEmbedder struct {
	dim    int
	phase  chan string
	next   chan struct{}
	mu     sync.Mutex
	closed bool
}

func (p *phaseEmbedder) Dim() int { return p.dim }

func (p *phaseEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	if err := p.signal(ctx, "entered"); err != nil {
		return nil, err
	}
	embed.EmitStep(ctx, "load_embed_done")
	if err := p.signal(ctx, "loaded"); err != nil {
		return nil, err
	}
	embed.EmitStep(ctx, "query_vector_done")
	if err := p.signal(ctx, "embedded"); err != nil {
		return nil, err
	}
	return embed.NewFixture(p.dim).Embed(ctx, texts)
}

func (p *phaseEmbedder) signal(ctx context.Context, name string) error {
	select {
	case p.phase <- name:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-p.next:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *phaseEmbedder) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *phaseEmbedder) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func mustEmbed(t *testing.T, emb embed.Embedder, text string) embed.Vector {
	t.Helper()
	vecs, err := emb.Embed(context.Background(), []string{text})
	if err != nil || len(vecs) != 1 {
		t.Fatal(err)
	}
	return vecs[0]
}

type parentBody map[int64]string

func (p parentBody) GetParent(ctx context.Context, id int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body, ok := p[id]
	if !ok {
		return "", errors.New("missing parent")
	}
	return body, nil
}

type phaseReranker struct {
	emb   *phaseEmbedder
	score float64
}

func TestTryKeepsEmbedAndSkipsSecondModel(t *testing.T) {
	emb := &residentEmbedder{inner: embed.NewFixture(8), pid: os.Getpid(), holding: true}
	rr := &countReranker{}
	h := residentTryHandler(t, emb, rr, retrieve.DefaultRerankMinScore, true)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	first := &lockedBuffer{}
	if err := readTryStream(ctx, srv.URL+"/api/try", "crimson lantern", first); err != nil {
		t.Fatal(err)
	}
	assertStepOrder(t, first.String(), []string{
		retrieve.StepLoadEmbed,
		retrieve.StepQueryVector,
		retrieve.StepVectorSearch,
		retrieve.StepBM25,
		retrieve.StepMergeTop50,
		retrieve.StepParkEmbed,
		retrieve.StepRerank,
		retrieve.StepFilter(retrieve.DefaultRerankMinScore),
		retrieve.StepParents,
	})
	if strings.Contains(first.String(), retrieve.StepEmbedResident) || strings.Contains(first.String(), retrieve.StepUnloadEmbed) || strings.Contains(first.String(), retrieve.StepLoadRerank) {
		t.Fatalf("first query reloaded or unloaded embed\n%s", first.String())
	}
	if !strings.Contains(first.String(), `"collection_id":"public"`) {
		t.Fatalf("result %s", first.String())
	}

	second := &lockedBuffer{}
	if err := readTryStream(ctx, srv.URL+"/api/try", "crimson lantern", second); err != nil {
		t.Fatal(err)
	}
	assertStepOrder(t, second.String(), []string{
		retrieve.StepEmbedResident,
		retrieve.StepQueryVector,
		retrieve.StepVectorSearch,
		retrieve.StepBM25,
		retrieve.StepMergeTop50,
		retrieve.StepRerank,
		retrieve.StepFilter(retrieve.DefaultRerankMinScore),
		retrieve.StepParents,
	})
	if strings.Contains(second.String(), retrieve.StepLoadEmbed) || strings.Contains(second.String(), retrieve.StepParkEmbed) || strings.Contains(second.String(), retrieve.StepLoadRerank) {
		t.Fatalf("second query reloaded embed or rerank\n%s", second.String())
	}
	if rr.n() != 0 {
		t.Fatalf("rerank model started %d times", rr.n())
	}
	if emb.wasClosed() {
		t.Fatal("embedder closed after the queries")
	}
}

func TestResidentTryDropsBelowThreshold(t *testing.T) {
	emb := &residentEmbedder{inner: embed.NewFixture(8), pid: os.Getpid(), holding: true}
	rr := &countReranker{}
	h := residentTryHandler(t, emb, rr, 10, true)
	raw, err := json.Marshal(map[string]any{"query": "crimson lantern"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/try", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("status %d %s", got.Code, got.Body.String())
	}
	if strings.TrimSpace(got.Body.String()) == "null" {
		t.Fatal("null hits")
	}
	var hits []retrieve.Hit
	if err := json.Unmarshal(got.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits %+v", hits)
	}
	if rr.n() != 0 || emb.wasClosed() {
		t.Fatalf("calls %d closed %v", rr.n(), emb.wasClosed())
	}
}

func residentTryHandler(t *testing.T, emb *residentEmbedder, passages retrieve.PassageReranker, min float64, park bool) http.Handler {
	t.Helper()
	ctx := context.Background()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	reg, err := index.NewDisk(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	disk := reg.(*index.Disk)
	qvec := mustEmbed(t, embed.NewFixture(meta.Dim), embed.QueryText("crimson lantern"))
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{{
		ChunkID: "c1", ParentID: 42, DocID: 7, CollectionID: "public",
		HeadingPath: "东桥", Body: "crimson lantern east bridge",
		StartByte: 3, EndByte: 9, Vector: qvec,
	}}); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		DataDir:        t.TempDir(),
		Weights:        retrieve.Weights{Vector: 0.7, Keyword: 0.3},
		Chunk:          chunk.Options{MaxLen: 800, Overlap: 100},
		RerankMinScore: min,
		KeepEmbed:      true,
		ParkEmbed:      park,
	}, job.Deps{
		Store:    store,
		Embedder: emb,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: passages,
		Parents:  parentBody{42: "父节正文"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Handler(p)
}

type residentEmbedder struct {
	inner   embed.Embedder
	pid     int
	holding bool
	mu      sync.Mutex
	loaded  bool
	closed  bool
}

func (r *residentEmbedder) Dim() int { return r.inner.Dim() }

func (r *residentEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	r.mu.Lock()
	first := !r.loaded
	r.mu.Unlock()
	if first {
		embed.EmitStep(ctx, "load_embed_done")
	}
	embed.EmitStep(ctx, "query_vector_done")
	vecs, err := r.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.loaded = true
	r.mu.Unlock()
	return vecs, nil
}

func (r *residentEmbedder) Loaded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded
}

func (r *residentEmbedder) ProcessID() int { return r.pid }

func (r *residentEmbedder) Holding() bool { return r.holding }

func (r *residentEmbedder) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *residentEmbedder) wasClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

type countReranker struct {
	mu    sync.Mutex
	calls int
}

func (c *countReranker) Score(ctx context.Context, _ string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return nil, errors.New("rerank model started")
}

func (c *countReranker) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (p *phaseReranker) Score(ctx context.Context, _ string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.emb == nil || !p.emb.isClosed() {
		return nil, errors.New("rerank while embedder is open")
	}
	out := make([]float64, len(passages))
	for i := range out {
		out[i] = p.score
	}
	return out, nil
}
