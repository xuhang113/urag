package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
	"github.com/xuhang113/urag/internal/web"
)

func TestPageUpload(t *testing.T) {
	h := acceptanceHandler(t)
	rec := get(t, h, "/")
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / status %d body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/upload" {
		t.Fatalf("GET / location %q", loc)
	}

	page := get(t, h, "/upload")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /upload status %d body %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "page-upload") || !strings.Contains(page.Body.String(), "上传") {
		t.Fatalf("upload page not recognizable: %s", page.Body.String())
	}

	payload := []byte("The crimson lantern festival is held on the east stone bridge.")
	body, ctype := uploadForm(t, "alpha", "upload-1", "note.txt", payload)
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	req.Header.Set("Content-Type", ctype)
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusAccepted {
		t.Fatalf("POST /api/uploads status %d body %s", got.Code, got.Body.String())
	}
	var resp struct {
		JobID        string `json:"job_id"`
		ObjectKey    string `json:"object_key"`
		Checksum     string `json:"checksum"`
		Size         int64  `json:"size"`
		CollectionID string `json:"collection_id"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil {
		t.Fatalf("upload json: %v body %s", err, got.Body.String())
	}
	sum := sha256.Sum256(payload)
	wantSum := hex.EncodeToString(sum[:])
	if resp.JobID == "" || resp.ObjectKey == "" || resp.Checksum == "" {
		t.Fatalf("upload response missing ids: %+v", resp)
	}
	if resp.Checksum != wantSum {
		t.Fatalf("checksum %s want %s", resp.Checksum, wantSum)
	}
	if resp.Size != int64(len(payload)) {
		t.Fatalf("size %d want %d", resp.Size, len(payload))
	}
	if resp.CollectionID != "alpha" {
		t.Fatalf("collection %s", resp.CollectionID)
	}
	jobRec := get(t, h, "/api/jobs/"+resp.JobID)
	if jobRec.Code != http.StatusOK {
		t.Fatalf("GET job status %d body %s", jobRec.Code, jobRec.Body.String())
	}
	var state job.Job
	if err := json.Unmarshal(jobRec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.State != job.StateSucceeded {
		t.Fatalf("inline job state %s error %s", state.State, state.Error)
	}
}

func TestPageFetch(t *testing.T) {
	h := acceptanceHandler(t)
	page := get(t, h, "/fetch")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /fetch status %d body %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "page-fetch") || !strings.Contains(page.Body.String(), "获取") {
		t.Fatalf("fetch page not recognizable: %s", page.Body.String())
	}

	raw, err := json.Marshal(map[string]string{
		"collection_id":      "alpha",
		"kind":               "local_dir",
		"uri":                t.TempDir(),
		"idempotency_prefix": "fetch-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/fetch", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusAccepted {
		t.Fatalf("POST /api/fetch status %d body %s", got.Code, got.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil {
		t.Fatalf("fetch json: %v body %s", err, got.Body.String())
	}
	if resp.JobID == "" {
		t.Fatalf("empty job_id body %s", got.Body.String())
	}
}

func TestPageClean(t *testing.T) {
	h := acceptanceHandler(t)
	page := get(t, h, "/clean")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /clean status %d body %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "page-clean") || !strings.Contains(page.Body.String(), "清洗") {
		t.Fatalf("clean page not recognizable: %s", page.Body.String())
	}

	payload := []byte("same body for dedupe")
	postUpload(t, h, "dedupe", "clean-a", payload)
	postUpload(t, h, "dedupe", "clean-b", payload)

	docsRec := get(t, h, "/api/collections/dedupe/documents")
	if docsRec.Code != http.StatusOK {
		t.Fatalf("documents status %d body %s", docsRec.Code, docsRec.Body.String())
	}
	var docs []model.Document
	if err := json.Unmarshal(docsRec.Body.Bytes(), &docs); err != nil {
		t.Fatalf("documents json: %v body %s", err, docsRec.Body.String())
	}
	var first model.DocumentID
	for _, doc := range docs {
		if !doc.Duplicate && doc.ID != "" {
			first = doc.ID
			break
		}
	}
	if first == "" {
		t.Fatalf("no original document: %+v", docs)
	}
	saw := false
	for _, doc := range docs {
		if doc.Duplicate && doc.Lineage.DuplicateOf == first {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("duplicate of %s not visible: %+v", first, docs)
	}
}

func TestPageAsk(t *testing.T) {
	h := acceptanceHandler(t)
	page := get(t, h, "/ask")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /ask status %d body %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "page-ask") || !strings.Contains(page.Body.String(), "提问") {
		t.Fatalf("ask page not recognizable: %s", page.Body.String())
	}

	original := []byte("The crimson lantern festival is held on the east stone bridge.")
	postUpload(t, h, "alpha", "ask-1", original)

	raw, err := json.Marshal(retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "crimson lantern festival",
		TopK:        5,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/retrieve", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("POST /api/retrieve status %d body %s", got.Code, got.Body.String())
	}
	var hits []retrieve.Hit
	if err := json.Unmarshal(got.Body.Bytes(), &hits); err != nil {
		t.Fatalf("hits json: %v body %s", err, got.Body.String())
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	hit := hits[0]
	if hit.CollectionID != "alpha" {
		t.Fatalf("collection %s", hit.CollectionID)
	}
	if hit.Text == "" {
		t.Fatal("empty hit text")
	}
	if hit.StartByte < 0 || hit.EndByte > len(original) || hit.StartByte > hit.EndByte {
		t.Fatalf("bad span %d:%d", hit.StartByte, hit.EndByte)
	}
	if string(original[hit.StartByte:hit.EndByte]) != hit.Text {
		t.Fatalf("span %q != hit %q", original[hit.StartByte:hit.EndByte], hit.Text)
	}
}

func TestPageEval(t *testing.T) {
	h := acceptanceHandler(t)
	page := get(t, h, "/eval")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /eval status %d body %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "page-eval") || !strings.Contains(page.Body.String(), "评价") {
		t.Fatalf("eval page not recognizable: %s", page.Body.String())
	}

	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/eval", nil))
	if post.Code != http.StatusOK {
		t.Fatalf("POST /api/eval status %d body %s", post.Code, post.Body.String())
	}
	assertEvalPayload(t, post.Body.Bytes())

	report := get(t, h, "/api/eval/report")
	if report.Code != http.StatusOK {
		t.Fatalf("GET /api/eval/report status %d body %s", report.Code, report.Body.String())
	}
	assertEvalPayload(t, report.Body.Bytes())
}

func assertEvalPayload(t *testing.T, raw []byte) {
	t.Helper()
	var payload struct {
		Normal   eval.Report `json:"normal"`
		Contrast eval.Report `json:"contrast"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("eval json: %v body %s", err, raw)
	}
	if payload.Normal.Direction != "baseline" {
		t.Fatalf("normal direction %s", payload.Normal.Direction)
	}
	switch payload.Contrast.Direction {
	case "up", "down", "flat":
	default:
		t.Fatalf("contrast direction %s", payload.Contrast.Direction)
	}
	for _, name := range []string{"normal", "contrast"} {
		rep := payload.Normal
		if name == "contrast" {
			rep = payload.Contrast
		}
		seen := map[string]eval.Score{}
		for _, score := range rep.Scores {
			seen[score.CollectionID] = score
		}
		for _, id := range []string{"alpha", "beta", "merged"} {
			score, ok := seen[id]
			if !ok {
				t.Fatalf("%s missing %s in %+v", name, id, rep.Scores)
			}
			if score.K <= 0 && name == "normal" {
				t.Fatalf("%s %s missing k: %+v", name, id, score)
			}
			_ = score.RecallAtK
			_ = score.MRR
		}
		if _, ok := seen["gamma"]; ok {
			t.Fatalf("%s included gamma: %+v", name, rep.Scores)
		}
	}
}

func acceptanceHandler(t *testing.T) http.Handler {
	t.Helper()
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		Inline:      true,
		DataDir:     t.TempDir(),
		Storage:     "local",
		FixturePath: fixturePath(t),
		Chunk:       chunk.Options{MaxLen: 800, Overlap: 100},
		Weights:     retrieve.Weights{Vector: 0.7, Keyword: 0.3},
	}, job.Deps{
		Store:    store,
		Embedder: embed.NewFixture(64),
		Registry: index.NewMemory(),
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	return web.Handler(p)
}

func fixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "recall", "fixture.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func uploadForm(t *testing.T, collection, idem, filename string, payload []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("collection", collection); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("idempotency_key", idem); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func TestPageTry(t *testing.T) {
	ctx := context.Background()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	reg, err := index.NewDisk(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reg.(*index.Disk)
	if !ok {
		t.Fatalf("%T", reg)
	}
	qvec := mustEmbed(t, emb, embed.QueryText("crimson lantern"))
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{
		{
			ChunkID: "c1", ParentID: 42, DocID: 7, CollectionID: "public",
			HeadingPath: "东桥", Body: "crimson lantern east bridge",
			StartByte: 3, EndByte: 9, Vector: qvec,
		},
		{
			ChunkID: "secret-1", ParentID: 99, DocID: 8, CollectionID: "secret",
			HeadingPath: "隐藏", Body: "crimson lantern secret",
			StartByte: 1, EndByte: 2, Vector: append(embed.Vector(nil), qvec...),
		},
	}); err != nil {
		t.Fatal(err)
	}
	opened := &openRecord{inner: reg}
	closer := &closeEmbedder{inner: emb}
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		Inline:  true,
		DataDir: t.TempDir(),
		Weights: retrieve.Weights{Vector: 0.7, Keyword: 0.3},
		Chunk:   chunk.Options{MaxLen: 800, Overlap: 100},
	}, job.Deps{
		Store:    store,
		Embedder: closer,
		Registry: opened,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: &closedReranker{emb: closer, score: 0.25},
		Parents:  parentBody{42: "父节正文"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := web.Handler(p)

	page := get(t, h, "/try")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /try status %d body %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, "page-try") || !strings.Contains(body, "public") || !strings.Contains(body, "/api/try") {
		t.Fatalf("try page: %s", body)
	}
	if !strings.Contains(body, "检索超时。收录正在占用嵌入模型，这次没有结果。") || !strings.Contains(body, "180000") {
		t.Fatalf("try page missing timeout: %s", body)
	}
	if !strings.Contains(body, "没有足够相关的片段") {
		t.Fatalf("try page missing empty copy: %s", body)
	}
	if !strings.Contains(body, `id="try-log"`) || !strings.Contains(body, "未完成") || !strings.Contains(body, "text/event-stream") {
		t.Fatalf("try page missing progress log: %s", body)
	}
	if !strings.Contains(body, "开始时间") || !strings.Contains(body, "结束时间") || !strings.Contains(body, "耗时") {
		t.Fatalf("try page missing step times: %s", body)
	}
	if !strings.Contains(body, "step.started") || !strings.Contains(body, "step.ended") || !strings.Contains(body, "step.seconds") {
		t.Fatalf("try page does not render step times: %s", body)
	}
	if !strings.Contains(body, "算完查询向量后卸下嵌入") || !strings.Contains(body, "再加载重排模型") || !strings.Contains(body, "嵌入设备") {
		t.Fatalf("try page missing sequential rerank: %s", body)
	}
	if !strings.Contains(body, `id="try-note"`) || !strings.Contains(body, "payload.note") || !strings.Contains(body, "hit.score") {
		t.Fatalf("try page missing hybrid score note: %s", body)
	}
	ask := get(t, h, "/ask")
	if ask.Code != http.StatusOK || !strings.Contains(ask.Body.String(), "page-ask") || !strings.Contains(ask.Body.String(), "提问") {
		t.Fatalf("ask page missing: %d %s", ask.Code, ask.Body.String())
	}

	empty := postJSON(t, h, "/api/try", map[string]any{"query": "  ", "collections": []string{"secret"}})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty query %d %s", empty.Code, empty.Body.String())
	}

	got := postJSON(t, h, "/api/try", map[string]any{
		"query":       "crimson lantern",
		"collections": []string{"secret"},
	})
	if got.Code != http.StatusOK {
		t.Fatalf("POST /api/try %d %s", got.Code, got.Body.String())
	}
	var hits []retrieve.Hit
	if err := json.Unmarshal(got.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %d", len(hits))
	}
	hit := hits[0]
	if hit.CollectionID != "public" || hit.ChunkID != "c1" || hit.HeadingPath != "东桥" {
		t.Fatalf("%+v", hit)
	}
	if hit.ChildText != "crimson lantern east bridge" || hit.ParentText != "父节正文" {
		t.Fatalf("chunk/parent %+v", hit)
	}
	if hit.StartByte != 3 || hit.EndByte != 9 || hit.Score != 0.25 {
		t.Fatalf("span/score %+v", hit)
	}
	if !closer.closed {
		t.Fatal("embedder still open after retrieve")
	}
	if len(opened.ids) != 1 || opened.ids[0] != "public" {
		t.Fatalf("opened %v", opened.ids)
	}
}

func TestPageTryDropsLowRerankScore(t *testing.T) {
	low := postPublicTry(t, retrieve.DefaultRerankMinScore, retrieve.DefaultRerankMinScore)
	if len(low) != 0 {
		t.Fatalf("score at the threshold was returned: %+v", low)
	}
	below := postPublicTry(t, 0.2, retrieve.DefaultRerankMinScore)
	if len(below) != 0 {
		t.Fatalf("low score was returned: %+v", below)
	}
	high := postPublicTry(t, retrieve.DefaultRerankMinScore+0.1, retrieve.DefaultRerankMinScore)
	if len(high) != 1 || high[0].ChunkID != "c1" || high[0].CollectionID != "public" {
		t.Fatalf("%+v", high)
	}
	if high[0].Score != retrieve.DefaultRerankMinScore+0.1 {
		t.Fatalf("score %+v", high[0])
	}
}

func postPublicTry(t *testing.T, score, min float64) []retrieve.Hit {
	t.Helper()
	ctx := context.Background()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	reg, err := index.NewDisk(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	disk := reg.(*index.Disk)
	qvec := mustEmbed(t, emb, embed.QueryText("crimson lantern"))
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{{
		ChunkID: "c1", ParentID: 42, DocID: 7, CollectionID: "public",
		HeadingPath: "东桥", Body: "crimson lantern east bridge",
		Vector: qvec,
	}}); err != nil {
		t.Fatal(err)
	}
	closer := &closeEmbedder{inner: emb}
	store, err := storage.NewLocal(t.TempDir(), storage.RetryPolicy{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := job.New(job.Config{
		Inline:         true,
		DataDir:        t.TempDir(),
		Weights:        retrieve.Weights{Vector: 0.7, Keyword: 0.3},
		Chunk:          chunk.Options{MaxLen: 800, Overlap: 100},
		RerankMinScore: min,
	}, job.Deps{
		Store:    store,
		Embedder: closer,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: &closedReranker{emb: closer, score: score},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := postJSON(t, web.Handler(p), "/api/try", map[string]any{"query": "crimson lantern"})
	if got.Code != http.StatusOK {
		t.Fatalf("POST /api/try %d %s", got.Code, got.Body.String())
	}
	if strings.TrimSpace(got.Body.String()) == "null" {
		t.Fatal("null hits")
	}
	var hits []retrieve.Hit
	if err := json.Unmarshal(got.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	return hits
}

func postJSON(t *testing.T, h http.Handler, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustEmbed(t *testing.T, emb embed.Embedder, text string) embed.Vector {
	t.Helper()
	vecs, err := emb.Embed(context.Background(), []string{text})
	if err != nil || len(vecs) != 1 {
		t.Fatal(err)
	}
	return vecs[0]
}

type openRecord struct {
	inner index.Registry
	ids   []model.CollectionID
}

func (o *openRecord) Open(ctx context.Context, id model.CollectionID) (index.Index, error) {
	o.ids = append(o.ids, id)
	return o.inner.Open(ctx, id)
}

func (o *openRecord) Get(id model.CollectionID) (index.Index, bool) {
	return o.inner.Get(id)
}

type closeEmbedder struct {
	inner  embed.Embedder
	closed bool
}

func (c *closeEmbedder) Dim() int { return c.inner.Dim() }

func (c *closeEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	return c.inner.Embed(ctx, texts)
}

func (c *closeEmbedder) Close() error {
	c.closed = true
	return nil
}

type closedReranker struct {
	emb   *closeEmbedder
	score float64
}

func (c *closedReranker) Score(ctx context.Context, query string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.emb == nil || !c.emb.closed {
		return nil, errors.New("rerank while embedder is open")
	}
	out := make([]float64, len(passages))
	for i := range out {
		out[i] = c.score
	}
	return out, nil
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

func postUpload(t *testing.T, h http.Handler, collection, idem string, payload []byte) {
	t.Helper()
	body, ctype := uploadForm(t, collection, idem, "note.txt", payload)
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload %s: status %d body %s", idem, rec.Code, rec.Body.String())
	}
}
