package web_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		Cleaner:  clean.New(nil),
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
