package index_test

import (
	"context"
	"math"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

func TestBM25HandScore(t *testing.T) {
	ctx := context.Background()
	reg := index.NewMemory()
	alpha, err := reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	gamma, err := reg.Open(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	chunks := []model.Chunk{
		{ID: "d1", DocumentID: "1", CollectionID: "alpha", Text: "crimson lantern"},
		{ID: "d2", DocumentID: "2", CollectionID: "alpha", Text: "crimson"},
	}
	vecs := []embed.Vector{{1}, {1}}
	if err := alpha.Upsert(ctx, chunks, vecs); err != nil {
		t.Fatal(err)
	}
	if err := gamma.Upsert(ctx, []model.Chunk{{
		ID: "secret", DocumentID: "9", CollectionID: "gamma", Text: "crimson lantern",
	}}, []embed.Vector{{1}}); err != nil {
		t.Fatal(err)
	}

	hits, err := alpha.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		Tokens:       []string{"Crimson", "lantern", "lantern"},
		TopK:         5,
		Rank:         index.RankBM25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Chunk.ID != "d1" || hits[1].Chunk.ID != "d2" {
		t.Fatalf("%+v", hits)
	}
	want1 := bm25Hand(2, 2, 1.5, true)
	want2 := bm25Hand(1, 1, 1.5, false)
	if math.Abs(hits[0].Score-want1) > 1e-9 || math.Abs(hits[1].Score-want2) > 1e-9 {
		t.Fatalf("scores %v %v, want %v %v", hits[0].Score, hits[1].Score, want1, want2)
	}
	for _, hit := range hits {
		if hit.Chunk.ID == "secret" || hit.CollectionID != "alpha" {
			t.Fatalf("%+v", hit)
		}
	}

	none, err := alpha.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		TopK:         5,
		Rank:         index.RankBM25,
	})
	if err != nil || len(none) != 0 {
		t.Fatalf("%+v %v", none, err)
	}
	if _, err := alpha.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		Tokens:       []string{"crimson"},
		TopK:         1,
		Rank:         "other",
	}); err == nil {
		t.Fatal("unknown rank should fail")
	}

	dir := t.TempDir()
	disk, err := index.NewDisk(dir, embed.ModelMeta{Name: "fixture", Dim: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = disk.(*index.Disk).UpsertRecords(ctx, []index.VectorRecord{
		{ChunkID: "d1", DocID: 1, CollectionID: "alpha", Body: "crimson lantern", Vector: embed.Vector{1}},
		{ChunkID: "d2", DocID: 2, CollectionID: "alpha", Body: "crimson", Vector: embed.Vector{1}},
		{ChunkID: "secret", DocID: 9, CollectionID: "gamma", Body: "crimson lantern", Vector: embed.Vector{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	diskAlpha, err := disk.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	diskHits, err := diskAlpha.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		Tokens:       []string{"crimson", "lantern"},
		TopK:         5,
		Rank:         index.RankBM25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(diskHits) != 2 || diskHits[0].Chunk.ID != "d1" || math.Abs(diskHits[0].Score-want1) > 1e-9 {
		t.Fatalf("%+v", diskHits)
	}
	if _, ok := disk.Get("gamma"); ok {
		t.Fatal("gamma was not opened")
	}
}

// bm25Hand 复算 d1 或 d2 的分数。
// 语料是 “crimson lantern” 和 “crimson”，平均长度 1.5。
// df(crimson)=2，df(lantern)=1。k1=1.2，b=0.75。
func bm25Hand(dl int, matched int, avg float64, both bool) float64 {
	const k1 = 1.2
	const b = 0.75
	n := 2.0
	idfCrimson := math.Log(1 + (n-2+0.5)/(2+0.5))
	idfLantern := math.Log(1 + (n-1+0.5)/(1+0.5))
	norm := 1 - b + b*float64(dl)/avg
	tf := (1 * (k1 + 1)) / (1 + k1*norm)
	score := idfCrimson * tf
	if both {
		score += idfLantern * tf
	}
	if matched < 0 {
		return 0
	}
	return score
}
