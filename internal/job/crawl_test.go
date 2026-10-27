package job

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
)

const (
	goodHTML = `<!doctype html><html><body>` +
		`<h1>Lantern</h1>` +
		`<p>The crimson lantern hangs on the east stone bridge.</p>` +
		`<h2>Gate</h2>` +
		`<p>The west granite gate stays shut.</p>` +
		`</body></html>`
	stoneHTML = `<!doctype html><html><body><h1>Stone</h1>` +
		`<p>A quiet cedar hall keeps the old map.</p></body></html>`
	bareHTML = `<!doctype html><html><body><h1>Bare Title</h1></body></html>`
	noteHTML = `<!doctype html><html><body><h1>Note</h1>` +
		`<p>The crimson lantern waits here.</p></body></html>`
)

// TestCrawlCountIndexedMatchesQualifiedDocuments 核对篇数口径：
// 清洗后 Duplicate 为 false，且至少有一个子块。catalog.CountIndexed 等于这些来源行。
func TestCrawlCountIndexedMatchesQualifiedDocuments(t *testing.T) {
	srv, client := newCorpus(t)
	defer srv.Close()
	rig := newCrawlRig(t, embed.NewFixture(64), true, 1)

	first, err := rig.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{srv.URL + "/index.html"},
		Client:       client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateSucceeded || first.Attempts != 1 {
		t.Fatalf("state %s attempts %d error %s", first.State, first.Attempts, first.Error)
	}
	if rig.store.puts != 0 {
		t.Fatalf("storage puts %d", rig.store.puts)
	}
	assertQualifiedCount(t, rig.cat, "alpha", 2)
	assertPassages(t, rig)
	assertDiskHit(t, rig, "crimson")
	for _, frag := range []string{"source_uris", "/good.html", "/copy.html", "/bare.html", "/flaky.html", "/missing.pdf", "/pic.png"} {
		if !strings.Contains(string(first.Checkpoint), frag) {
			t.Fatalf("checkpoint %s missing %s", first.Checkpoint, frag)
		}
	}
	ids := ledgerIDs(t, rig.cat)
	before := append([]int64(nil), ids...)

	second, err := rig.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{srv.URL + "/index.html"},
		Client:       client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != StateSucceeded {
		t.Fatalf("second state %s error %s", second.State, second.Error)
	}
	assertQualifiedCount(t, rig.cat, "alpha", 2)
	after := ledgerIDs(t, rig.cat)
	if len(after) != len(before) || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("doc ids %v then %v", before, after)
	}
	if docsContaining(t, rig.cat, after, "crimson") != 1 {
		t.Fatal("same source produced another ledger row")
	}
	if rig.store.puts != 0 {
		t.Fatalf("storage puts %d", rig.store.puts)
	}
	beta, err := rig.cat.CountIndexed(context.Background(), "beta")
	if err != nil || beta != 0 {
		t.Fatalf("beta %d err %v", beta, err)
	}
}

func TestCrawlFailureRetriesThreeTimesAndKeepsOneRow(t *testing.T) {
	seed, client := serveNote(t, noteHTML)
	flaky := &flakyEmbedder{inner: embed.NewFixture(64), failUntil: 2}
	rig := newCrawlRig(t, flaky, true, 16)
	got, err := rig.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{seed},
		Client:       client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded || got.Attempts != 3 {
		t.Fatalf("state %s attempts %d error %s calls %d", got.State, got.Attempts, got.Error, flaky.calls)
	}
	if flaky.calls != 3 {
		t.Fatalf("embed calls %d", flaky.calls)
	}
	assertQualifiedCount(t, rig.cat, "alpha", 1)
	ids := ledgerIDs(t, rig.cat)
	if len(ids) != 1 {
		t.Fatalf("docs %v", ids)
	}
	kids, err := rig.cat.ListChildren(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].ChunkID != catalog.ChunkID(ids[0], kids[0].Ordinal) {
		t.Fatalf("children %+v", kids)
	}
	if _, err := rig.cat.ListChildren(context.Background(), ids[0]+1); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("extra doc: %v", err)
	}
	if rig.store.puts != 0 {
		t.Fatalf("storage puts %d", rig.store.puts)
	}
}

func TestCrawlCancelDoesNotRetry(t *testing.T) {
	seed, client := serveNote(t, noteHTML)
	gate := &gateEmbedder{inner: embed.NewFixture(64), started: make(chan struct{})}
	rig := newCrawlRig(t, gate, false, 16)
	accepted, err := rig.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{seed},
		Client:       client,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("embed did not start")
	}
	if err := rig.p.Cancel(context.Background(), accepted.ID); err != nil {
		t.Fatal(err)
	}
	waitCrawl(t, rig.p, accepted.ID)
	got, err := rig.p.GetJob(context.Background(), accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCanceled || got.Attempts != 1 || gate.calls() != 1 {
		t.Fatalf("state %s attempts %d calls %d error %s", got.State, got.Attempts, gate.calls(), got.Error)
	}
	if rig.store.puts != 0 {
		t.Fatalf("storage puts %d", rig.store.puts)
	}
}

func TestCrawlRequiresLedgerAndVectors(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	_, err := lab.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{"http://127.0.0.1/sitemap.xml"},
	})
	if !errors.Is(err, errMissingDeps) {
		t.Fatalf("err %v", err)
	}
	if lab.store.puts != 0 {
		t.Fatalf("storage puts %d", lab.store.puts)
	}
}

func TestCrawlCheckpointFileSkipsCompletedSources(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	count := func(path string) {
		mu.Lock()
		hits[path]++
		mu.Unlock()
	}
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			count(r.URL.Path)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/a.html", page(`<!doctype html><html><body><h1>Lantern</h1><p>The crimson lantern hangs on the east stone bridge.</p></body></html>`))
	mux.HandleFunc("/b.html", page(`<!doctype html><html><body><h1>Gate</h1><p>The west granite gate stays shut.</p></body></html>`))
	var srv *httptest.Server
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		count(r.URL.Path)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>` + srv.URL + `/a.html</loc></url><url><loc>` + srv.URL + `/b.html</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ck := filepath.Join(t.TempDir(), "crawl-checkpoint.json")
	first := newCrawlRig(t, embed.NewFixture(64), true, 16)
	first.p.cfg.CrawlCheckpoint = ck
	got, err := first.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{srv.URL + "/sitemap.xml"},
		Client:       srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded {
		t.Fatalf("state %s error %s", got.State, got.Error)
	}
	raw, err := os.ReadFile(ck)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"source_uris", srv.URL + "/a.html", srv.URL + "/b.html"} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("checkpoint %s missing %s", raw, frag)
		}
	}
	mu.Lock()
	seenA, seenB := hits["/a.html"], hits["/b.html"]
	mu.Unlock()
	if seenA == 0 || seenB == 0 {
		t.Fatalf("first fetch a=%d b=%d", seenA, seenB)
	}

	second := newCrawlRig(t, embed.NewFixture(64), true, 16)
	second.p.cfg.CrawlCheckpoint = ck
	again, err := second.p.SubmitCrawl(context.Background(), CrawlInput{
		CollectionID: "alpha",
		Seeds:        []string{srv.URL + "/sitemap.xml"},
		Client:       srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.State != StateSucceeded {
		t.Fatalf("second state %s error %s", again.State, again.Error)
	}
	if len(second.emb.batches) != 0 {
		t.Fatalf("completed sources were embedded again: %d", len(second.emb.batches))
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/a.html"] != seenA || hits["/b.html"] != seenB {
		t.Fatalf("refetch a %d->%d b %d->%d", seenA, hits["/a.html"], seenB, hits["/b.html"])
	}
}

func TestCrawlFileDoesNotBufferOriginal(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "crawl.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{"io.ReadAll", "storage.Put", "NewQwen"} {
		if strings.Contains(text, frag) {
			t.Fatalf("crawl.go contains %s", frag)
		}
	}
}

func newCorpus(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	var mu sync.Mutex
	gets := map[string]int{}
	mux := http.NewServeMux()
	html := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/index.html", html(`<!doctype html><html><body><nav>`+
		`<a href="good.html">good</a>`+
		`<a href="copy.html">copy</a>`+
		`<a href="stone.html">stone</a>`+
		`<a href="bare.html">bare</a>`+
		`<a href="flaky.html">flaky</a>`+
		`<a href="missing.pdf">missing</a>`+
		`<a href="pic.png">pic</a>`+
		`</nav></body></html>`))
	mux.HandleFunc("/good.html", html(goodHTML))
	mux.HandleFunc("/copy.html", html(goodHTML))
	mux.HandleFunc("/stone.html", html(stoneHTML))
	mux.HandleFunc("/bare.html", html(bareHTML))
	mux.HandleFunc("/flaky.html", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gets[r.URL.Path]++
		n := gets[r.URL.Path]
		mu.Unlock()
		if n >= 2 {
			http.Error(w, "download failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(noteHTML))
	})
	mux.HandleFunc("/missing.pdf", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("png was requested")
	})
	srv := httptest.NewServer(mux)
	return srv, loopbackClient(t, srv)
}

func serveNote(t *testing.T, body string) (string, *http.Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/note.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/note.html", loopbackClient(t, srv)
}

func loopbackClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	client := srv.Client()
	base := client.Transport
	client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		ip := net.ParseIP(r.URL.Hostname())
		if ip == nil || !ip.IsLoopback() {
			t.Errorf("non-loopback request %s", r.URL.String())
			return nil, errors.New("job test: non-loopback")
		}
		return base.RoundTrip(r)
	})
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type crawlRig struct {
	p     *Pipeline
	store *fakeStore
	cat   catalog.Store
	reg   index.Registry
	emb   *batchEmbedder
}

func newCrawlRig(t *testing.T, inner embed.Embedder, inline bool, batch int) *crawlRig {
	t.Helper()
	if inner.Dim() != 64 {
		t.Fatalf("dim %d", inner.Dim())
	}
	store := newFakeStore()
	cat := catalog.NewMemory()
	reg, err := index.NewDisk(t.TempDir(), embed.ModelMeta{Name: "fixture", Dim: 64})
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reg.(*index.Disk)
	if !ok {
		t.Fatalf("%T", reg)
	}
	wrapped := &batchEmbedder{inner: inner}
	p, err := New(Config{
		Inline: inline,
		Retry:  storage.RetryPolicy{MaxAttempts: 3},
		Limits: Limits{EmbedBatch: batch},
		Chunk:  chunk.Options{MaxLen: 800, Overlap: 100},
		Weights: retrieve.Weights{
			Vector:  0.7,
			Keyword: 0.3,
		},
	}, Deps{
		Store:    store,
		Embedder: wrapped,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Catalog:  cat,
		Vectors:  disk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &crawlRig{p: p, store: store, cat: cat, reg: reg, emb: wrapped}
}

func assertQualifiedCount(t *testing.T, cat catalog.Store, collection string, want int) {
	t.Helper()
	ids := ledgerIDs(t, cat)
	if len(ids) != want {
		t.Fatalf("qualified docs %v", ids)
	}
	n, err := cat.CountIndexed(context.Background(), collection)
	if err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("CountIndexed %d want %d", n, want)
	}
}

func ledgerIDs(t *testing.T, cat catalog.Store) []int64 {
	t.Helper()
	var ids []int64
	ctx := context.Background()
	for id := int64(1); id <= 12; id++ {
		kids, err := cat.ListChildren(ctx, id)
		if errors.Is(err, catalog.ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(kids) == 0 {
			t.Fatalf("doc %d is indexed without children", id)
		}
		for _, kid := range kids {
			if kid.ChunkID != catalog.ChunkID(id, kid.Ordinal) {
				t.Fatalf("chunk_id %s want %s", kid.ChunkID, catalog.ChunkID(id, kid.Ordinal))
			}
		}
		ids = append(ids, id)
	}
	return ids
}

func docsContaining(t *testing.T, cat catalog.Store, ids []int64, phrase string) int {
	t.Helper()
	n := 0
	for _, id := range ids {
		kids, err := cat.ListChildren(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, kid := range kids {
			if strings.Contains(kid.Body, phrase) {
				n++
				break
			}
		}
	}
	return n
}

func assertPassages(t *testing.T, rig *crawlRig) {
	t.Helper()
	var want []string
	for _, id := range ledgerIDs(t, rig.cat) {
		kids, err := rig.cat.ListChildren(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, kid := range kids {
			want = append(want, chunk.Passage(kid.HeadingPath, kid.Body))
		}
	}
	rig.emb.mu.Lock()
	batches := append([][]string(nil), rig.emb.batches...)
	rig.emb.mu.Unlock()
	if len(batches) != len(want) {
		t.Fatalf("embed batches %d children %d", len(batches), len(want))
	}
	got := map[string]int{}
	for _, batch := range batches {
		if len(batch) != 1 {
			t.Fatalf("batch size %d", len(batch))
		}
		got[batch[0]]++
	}
	for _, text := range want {
		if got[text] != 1 {
			t.Fatalf("missing passage %q in %v", text, got)
		}
	}
}

func assertDiskHit(t *testing.T, rig *crawlRig, phrase string) {
	t.Helper()
	ctx := context.Background()
	var want catalog.Child
	for _, id := range ledgerIDs(t, rig.cat) {
		kids, err := rig.cat.ListChildren(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, kid := range kids {
			if strings.Contains(kid.Body, phrase) {
				want = kid
			}
		}
	}
	if want.ChunkID == "" {
		t.Fatalf("no child contains %s", phrase)
	}
	idx, err := rig.reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		Tokens:       []string{phrase},
		TopK:         5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if string(hit.Chunk.ID) == want.ChunkID && hit.ParentID == want.ParentID && hit.HeadingPath == want.HeadingPath {
			return
		}
	}
	t.Fatalf("disk hits %+v want %s parent %d", hits, want.ChunkID, want.ParentID)
}

func waitCrawl(t *testing.T, p *Pipeline, id string) {
	t.Helper()
	p.mu.Lock()
	rec := p.jobs[id]
	p.mu.Unlock()
	if rec == nil || rec.done == nil {
		t.Fatal("missing job")
	}
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("crawl did not finish")
	}
}

type batchEmbedder struct {
	inner   embed.Embedder
	mu      sync.Mutex
	batches [][]string
}

func (b *batchEmbedder) Dim() int { return b.inner.Dim() }

func (b *batchEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	b.mu.Lock()
	b.batches = append(b.batches, append([]string(nil), texts...))
	b.mu.Unlock()
	return b.inner.Embed(ctx, texts)
}

type flakyEmbedder struct {
	inner     embed.Embedder
	failUntil int
	mu        sync.Mutex
	calls     int
}

func (f *flakyEmbedder) Dim() int { return f.inner.Dim() }

func (f *flakyEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.failUntil {
		return nil, errors.New("embed unavailable")
	}
	return f.inner.Embed(ctx, texts)
}

type gateEmbedder struct {
	inner   embed.Embedder
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	n       int
}

func (g *gateEmbedder) Dim() int { return g.inner.Dim() }

func (g *gateEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	g.once.Do(func() { close(g.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (g *gateEmbedder) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}
