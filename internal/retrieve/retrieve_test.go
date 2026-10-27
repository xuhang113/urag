package retrieve_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

func TestRetrieveRequiresCollections(t *testing.T) {
	svc := retrieve.New(index.NewMemory(), embed.NewFixture(8), retrieve.Weights{}, nil)
	_, err := svc.Retrieve(context.Background(), retrieve.Request{Query: "crimson", TopK: 5})
	if !errors.Is(err, retrieve.ErrCollectionsRequired) {
		t.Fatal(err)
	}
}

func TestRetrieveHybridHandScore(t *testing.T) {
	ctx := context.Background()
	reg := index.NewMemory()
	idx, err := reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	emb := embed.NewFixture(64)
	chunks := []model.Chunk{
		{ID: "red#0", DocumentID: "red", CollectionID: "alpha", Text: "red", StartByte: 0, EndByte: 3},
		{ID: "blue#0", DocumentID: "blue", CollectionID: "alpha", Text: "blue", StartByte: 0, EndByte: 4},
	}
	vecs, err := emb.Embed(ctx, []string{"red", "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, chunks, vecs); err != nil {
		t.Fatal(err)
	}
	svc := retrieve.New(reg, emb, retrieve.Weights{Vector: 0.5, Keyword: 0.5}, nil)
	hits, err := svc.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "red",
		TopK:        5,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	h := hits[0]
	if h.DocumentID != "red" || h.CollectionID != "alpha" || h.ChunkID != "red#0" || h.Text != "red" {
		t.Fatalf("%+v", h)
	}
	if h.StartByte != 0 || h.EndByte != 3 || h.Score != 1 {
		t.Fatalf("score=%v bytes=%d:%d", h.Score, h.StartByte, h.EndByte)
	}

	def := retrieve.New(reg, emb, retrieve.Weights{}, nil)
	hits, err = def.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "red",
		TopK:        5,
		Hybrid:      true,
	})
	if err != nil || len(hits) != 1 || math.Abs(hits[0].Score-1) > 1e-9 {
		t.Fatalf("%+v %v", hits, err)
	}
}

func TestOverlapRerankerHandScore(t *testing.T) {
	rr := retrieve.NewOverlapReranker()
	hits := []retrieve.Hit{
		{DocumentID: "silver", ChunkID: "silver#0", Text: "silver", Score: 4, ParentID: 0, HeadingPath: ""},
		{DocumentID: "crimson", ChunkID: "crimson#0", Text: "crimson", Score: 0, ParentID: 0, HeadingPath: ""},
	}
	out, err := rr.Rerank(context.Background(), "Crimson!", hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].DocumentID != "crimson" || out[0].Score != 1001 {
		t.Fatalf("%+v", out)
	}
	if out[1].DocumentID != "silver" || out[1].Score != 0 {
		t.Fatalf("second %+v", out[1])
	}
	if hits[0].Score != 4 || hits[0].DocumentID != "silver" || hits[1].Score != 0 {
		t.Fatal("rerank mutated the input")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rr.Rerank(ctx, "crimson", hits); err == nil {
		t.Fatal("canceled context should fail")
	}
	if hits[0].Score != 4 {
		t.Fatal("failed rerank mutated the input")
	}
}

func TestRetrieveRerankOrdersByOverlap(t *testing.T) {
	ctx := context.Background()
	reg := index.NewMemory()
	idx, err := reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	emb := embed.NewFixture(64)
	chunks := []model.Chunk{
		{ID: "wide#0", DocumentID: "wide", CollectionID: "alpha", Text: "crimson lantern extra", StartByte: 0, EndByte: 21},
		{ID: "exact#0", DocumentID: "exact", CollectionID: "alpha", Text: "crimson", StartByte: 0, EndByte: 7},
	}
	vecs, err := emb.Embed(ctx, []string{chunks[0].Text, chunks[1].Text})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, chunks, vecs); err != nil {
		t.Fatal(err)
	}
	svc := retrieve.New(reg, emb, retrieve.Weights{}, retrieve.NewOverlapReranker())
	hits, err := svc.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "crimson",
		TopK:        2,
		Hybrid:      true,
		Rerank:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].DocumentID != "exact" || hits[0].Score != 1001 {
		t.Fatalf("%+v", hits)
	}
	if hits[0].CollectionID != "alpha" || hits[1].CollectionID != "alpha" {
		t.Fatalf("%+v", hits)
	}
}

func TestHitJSONParentFields(t *testing.T) {
	raw, err := json.Marshal(retrieve.Hit{
		CollectionID: "alpha",
		DocumentID:   "7",
		ChunkID:      "7#0",
		Text:         "body",
		Score:        1,
		StartByte:    1,
		EndByte:      2,
		ParentID:     9,
		HeadingPath:  "A/B",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"collection_id", "document_id", "chunk_id", "text", "score", "start_byte", "end_byte", "parent_id", "heading_path"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %s", key, raw)
		}
	}
	if got["parent_id"] != float64(9) || got["heading_path"] != "A/B" {
		t.Fatalf("%s", raw)
	}
}
