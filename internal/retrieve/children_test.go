package retrieve_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

func TestNewQwenRerankerMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-reranker")
	_, err := retrieve.NewQwenReranker(dir)
	if err == nil {
		t.Fatal("missing reranker directory should fail")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("missing directory was created")
	}
}

func TestRetrieveChildrenRequiresInput(t *testing.T) {
	svc := retrieve.New(index.NewMemory(), embed.NewFixture(8), retrieve.Weights{}, nil)
	_, err := svc.RetrieveChildren(context.Background(), retrieve.Request{}, scoreReranker{}, nil)
	if !errors.Is(err, retrieve.ErrCollectionsRequired) {
		t.Fatal(err)
	}
	_, err = svc.RetrieveChildren(context.Background(), retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "crimson",
	}, nil, nil)
	if err == nil {
		t.Fatal("nil reranker should fail")
	}
}

func TestRetrieveChildrenRerankAndParent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	reg, err := index.NewDisk(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reg.(*index.Disk)
	if !ok {
		t.Fatalf("%T", reg)
	}
	shared := mustOne(t, emb, "shared fixture vector")

	var recs []index.VectorRecord
	for docID := 1000; docID < 1010; docID++ {
		recs = append(recs, childRecord(docID, "alpha", fmt.Sprintf("crimson lowrank %d", docID), "", shared))
	}
	recs = append(recs, childRecord(1010, "beta", "crimson lantern itembeta", "Sec beta", shared))
	for docID := 1020; docID < 1070; docID++ {
		heading := fmt.Sprintf("Sec %d", docID)
		if docID == 1023 {
			heading = ""
		}
		recs = append(recs, childRecord(docID, "alpha", fmt.Sprintf("crimson lantern item%d", docID), heading, shared))
	}
	recs = append(recs, childRecord(3, "gamma", "gamma only token", "Secret", shared))
	if err := disk.UpsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}

	opened := &recordingRegistry{inner: reg}
	queryEmb := &recordingEmbedder{inner: emb}
	svc := retrieve.New(opened, queryEmb, retrieve.Weights{}, nil)
	rr := &captureReranker{}
	parents := &parentMap{body: map[int64]string{}, miss: map[int64]bool{}}
	for docID := 1020; docID <= 1026; docID++ {
		parents.body[int64(docID)+8000] = fmt.Sprintf("父节 %d", docID)
	}
	parents.miss[1027+8000] = true

	const question = "crimson lantern"
	hits, err := svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       question,
		TopK:        3,
		Hybrid:      false,
		Rerank:      false,
	}, rr, parents)
	if err != nil {
		t.Fatal(err)
	}
	if rr.calls != 1 || len(rr.passages) != 50 {
		t.Fatalf("rerank calls %d passages %d", rr.calls, len(rr.passages))
	}
	if rr.query != embed.QueryText(question) {
		t.Fatalf("query %q", rr.query)
	}
	if len(queryEmb.got) != 1 || len(queryEmb.got[0]) != 1 || queryEmb.got[0][0] != embed.QueryText(question) {
		t.Fatalf("embed calls %+v", queryEmb.got)
	}
	for _, passage := range rr.passages {
		if strings.Contains(passage, "lowrank") || strings.Contains(passage, "itembeta") || strings.Contains(passage, "gamma only") {
			t.Fatalf("reranker saw %q", passage)
		}
		if strings.HasPrefix(passage, "Instruct:") {
			t.Fatalf("passage has query prefix %q", passage)
		}
	}
	plain := "crimson lantern item1023"
	headed := "Sec 1020\ncrimson lantern item1020"
	if !containsString(rr.passages, plain) || !containsString(rr.passages, headed) {
		t.Fatalf("passages missing embed text")
	}
	for _, id := range opened.opened {
		if id != "alpha" {
			t.Fatalf("opened %v", opened.opened)
		}
	}
	if len(opened.opened) != 1 {
		t.Fatalf("opened %v", opened.opened)
	}
	if len(hits) != 8 {
		t.Fatalf("hits %d", len(hits))
	}
	if len(parents.ids) != 8 {
		t.Fatalf("parent lookups %v", parents.ids)
	}
	if hits[0].DocumentID != "1020" || hits[0].Score != 50 || hits[0].CollectionID != "alpha" || hits[0].ChunkID != "c1020" {
		t.Fatalf("%+v", hits[0])
	}
	if hits[0].Text != "父节 1020" || hits[0].HeadingPath != "Sec 1020" || hits[0].ParentID != 9020 {
		t.Fatalf("%+v", hits[0])
	}
	if hits[0].StartByte != 0 || hits[0].EndByte != 0 {
		t.Fatalf("bytes %+v", hits[0])
	}
	if hits[3].DocumentID != "1023" || hits[3].HeadingPath != "" || hits[3].Text != "父节 1023" {
		t.Fatalf("%+v", hits[3])
	}
	if hits[7].DocumentID != "1027" || hits[7].Text != "crimson lantern item1027" || hits[7].ParentID != 9027 {
		t.Fatalf("%+v", hits[7])
	}
	for _, hit := range hits {
		if hit.CollectionID == "" || hit.DocumentID == "" || hit.ChunkID == "" || hit.Text == "" {
			t.Fatalf("%+v", hit)
		}
		if hit.CollectionID != "alpha" {
			t.Fatalf("%+v", hit)
		}
	}
}

func TestRetrieveChildrenKeepsShortList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	reg, err := index.NewDisk(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	disk := reg.(*index.Disk)
	shared := mustOne(t, emb, "shared fixture vector")
	var recs []index.VectorRecord
	for _, docID := range []int{1, 2, 3} {
		recs = append(recs, childRecord(docID, "alpha", fmt.Sprintf("crimson lantern item%d", docID), "H", shared))
	}
	if err := disk.UpsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	svc := retrieve.New(reg, emb, retrieve.Weights{}, nil)
	rr := &captureReranker{}
	hits, err := svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "crimson lantern",
		TopK:        1,
	}, rr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || len(rr.passages) != 3 {
		t.Fatalf("hits %d passages %d", len(hits), len(rr.passages))
	}
	for _, hit := range hits {
		if !strings.HasPrefix(hit.Text, "crimson lantern item") {
			t.Fatalf("nil parents replaced text %+v", hit)
		}
	}
	parents := &parentMap{body: map[int64]string{8001: "父节 1", 8002: "父节 2", 8003: "父节 3"}}
	hits, err = svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "crimson lantern",
	}, rr, parents)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].Text != "父节 1" || hits[2].Text != "父节 3" {
		t.Fatalf("%+v", hits)
	}
}

type captureReranker struct {
	calls    int
	query    string
	passages []string
}

func (c *captureReranker) Score(ctx context.Context, query string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.calls++
	c.query = query
	c.passages = append([]string(nil), passages...)
	out := make([]float64, len(passages))
	for i := range out {
		out[i] = float64(len(passages) - i)
	}
	return out, nil
}

type scoreReranker struct{}

func (scoreReranker) Score(context.Context, string, []string) ([]float64, error) {
	return nil, errors.New("unused")
}

type parentMap struct {
	body map[int64]string
	miss map[int64]bool
	ids  []int64
}

func (p *parentMap) GetParent(ctx context.Context, id int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.ids = append(p.ids, id)
	if p.miss[id] {
		return "", errors.New("not found")
	}
	body, ok := p.body[id]
	if !ok {
		return "", errors.New("not found")
	}
	return body, nil
}

type recordingEmbedder struct {
	inner embed.Embedder
	got   [][]string
}

func (r *recordingEmbedder) Dim() int { return r.inner.Dim() }

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	r.got = append(r.got, append([]string(nil), texts...))
	return r.inner.Embed(ctx, texts)
}

type recordingRegistry struct {
	inner  index.Registry
	opened []model.CollectionID
}

func (r *recordingRegistry) Open(ctx context.Context, id model.CollectionID) (index.Index, error) {
	r.opened = append(r.opened, id)
	return r.inner.Open(ctx, id)
}

func (r *recordingRegistry) Get(id model.CollectionID) (index.Index, bool) {
	return r.inner.Get(id)
}

func childRecord(docID int, collection, body, heading string, vec embed.Vector) index.VectorRecord {
	return index.VectorRecord{
		ChunkID:      fmt.Sprintf("c%d", docID),
		ParentID:     int64(docID) + 8000,
		DocID:        int64(docID),
		Version:      1,
		CollectionID: model.CollectionID(collection),
		HeadingPath:  heading,
		Page:         2,
		Body:         body,
		Vector:       append(embed.Vector(nil), vec...),
	}
}

func mustOne(t *testing.T, emb embed.Embedder, text string) embed.Vector {
	t.Helper()
	vecs, err := emb.Embed(context.Background(), []string{text})
	if err != nil || len(vecs) != 1 {
		t.Fatal(err)
	}
	return vecs[0]
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
