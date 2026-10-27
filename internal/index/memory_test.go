package index_test

import (
	"context"
	"math"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

func TestMemoryIndex(t *testing.T) {
	ctx := context.Background()
	reg := index.NewMemory()
	if _, ok := reg.Get("alpha"); ok {
		t.Fatal("alpha opened before Open")
	}
	alpha, err := reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Collection() != "alpha" {
		t.Fatal(alpha.Collection())
	}
	again, err := reg.Open(ctx, "alpha")
	if err != nil || again.Collection() != "alpha" {
		t.Fatal(err)
	}

	emb := embed.NewFixture(64)
	chunks := []model.Chunk{
		{ID: "alpha-lantern#0", DocumentID: "alpha-lantern", CollectionID: "alpha", Text: "crimson lantern", StartByte: 0, EndByte: 15},
		{ID: "alpha-festival#0", DocumentID: "alpha-festival", CollectionID: "alpha", Text: "festival drums", StartByte: 0, EndByte: 14},
		{ID: "alpha-bridge#0", DocumentID: "alpha-bridge", CollectionID: "alpha", Text: "stone bridge", StartByte: 0, EndByte: 12},
	}
	texts := []string{chunks[0].Text, chunks[1].Text, chunks[2].Text}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	if err := alpha.Upsert(ctx, chunks, vecs); err != nil {
		t.Fatal(err)
	}
	if err := alpha.Upsert(ctx, chunks[:1], nil); err == nil {
		t.Fatal("length mismatch should fail")
	}
	fresh := model.Chunk{ID: "fresh#0", DocumentID: "fresh", CollectionID: "alpha", Text: "fresh token"}
	bad := model.Chunk{ID: "bad#0", DocumentID: "bad", CollectionID: "gamma", Text: "gamma only token"}
	badVecs, err := emb.Embed(ctx, []string{fresh.Text, bad.Text})
	if err != nil {
		t.Fatal(err)
	}
	if err := alpha.Upsert(ctx, []model.Chunk{fresh, bad}, badVecs); err == nil {
		t.Fatal("foreign chunk should fail the batch")
	}

	q := index.Query{
		CollectionID: "alpha",
		Tokens:       []string{"crimson", "lantern", "festival"},
		TopK:         10,
	}
	kw, err := alpha.SearchKeyword(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(kw) != 2 {
		t.Fatalf("keyword hits %d", len(kw))
	}
	if kw[0].Chunk.DocumentID != "alpha-lantern" || math.Abs(kw[0].Score-2.0/3.0) > 1e-12 {
		t.Fatalf("top keyword %+v", kw[0])
	}
	if kw[1].Chunk.DocumentID != "alpha-festival" || math.Abs(kw[1].Score-1.0/3.0) > 1e-12 {
		t.Fatalf("second keyword %+v", kw[1])
	}
	for _, h := range kw {
		if h.Score <= 0 || h.CollectionID != "alpha" {
			t.Fatalf("bad hit %+v", h)
		}
	}
	q.TopK = 1
	top, err := alpha.SearchKeyword(ctx, q)
	if err != nil || len(top) != 1 || top[0].Chunk.DocumentID != "alpha-lantern" {
		t.Fatalf("topk %+v %v", top, err)
	}
	q.Tokens = nil
	q.TopK = 10
	none, err := alpha.SearchKeyword(ctx, q)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty query %+v %v", none, err)
	}

	q.CollectionID = "beta"
	if _, err := alpha.SearchKeyword(ctx, q); err == nil {
		t.Fatal("keyword search should reject another collection")
	}
	if _, err := alpha.SearchVector(ctx, q); err == nil {
		t.Fatal("vector search should reject another collection")
	}

	q = index.Query{CollectionID: "alpha", Vector: vecs[0], TopK: 10}
	vh, err := alpha.SearchVector(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(vh) == 0 || vh[0].Chunk.DocumentID != "alpha-lantern" || math.Abs(vh[0].Score-1) > 1e-6 {
		t.Fatalf("vector top %+v", vh)
	}
	for _, h := range vh {
		if h.Chunk.DocumentID == "fresh" || h.Chunk.DocumentID == "bad" {
			t.Fatalf("rejected batch was written: %+v", h)
		}
		if h.Score <= 0 {
			t.Fatalf("non-positive score %+v", h)
		}
	}

	beta, err := reg.Open(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	betaChunk := model.Chunk{ID: "beta-harbor#0", DocumentID: "beta-harbor", CollectionID: "beta", Text: "crimson lantern"}
	betaVec, err := emb.Embed(ctx, []string{betaChunk.Text})
	if err != nil {
		t.Fatal(err)
	}
	if err := beta.Upsert(ctx, []model.Chunk{betaChunk}, betaVec); err != nil {
		t.Fatal(err)
	}
	q = index.Query{CollectionID: "alpha", Tokens: []string{"crimson", "lantern"}, TopK: 10}
	onlyAlpha, err := alpha.SearchKeyword(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range onlyAlpha {
		if h.Chunk.DocumentID == "beta-harbor" || h.CollectionID != "alpha" {
			t.Fatalf("alpha search returned %+v", h)
		}
	}
	got, ok := reg.Get("gamma")
	if ok || got != nil {
		t.Fatal("gamma should stay closed")
	}
}
