package index_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

func TestDiskRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 64}
	emb := embed.NewFixture(meta.Dim)
	disk := openDisk(t, dir, meta)

	const n = 40
	texts := make([]string, n)
	for i := range texts {
		texts[i] = fmt.Sprintf("crimson lantern %c%c", 'a'+i/26, 'a'+i%26)
	}
	vecs, err := emb.Embed(ctx, append(append([]string{}, texts...), ""))
	if err != nil {
		t.Fatal(err)
	}
	recs := make([]index.VectorRecord, n+1)
	for i := 0; i < n; i++ {
		recs[i] = index.VectorRecord{
			ChunkID:      fmt.Sprintf("c%02d", i),
			ParentID:     int64(100 + i),
			DocID:        int64(i + 1),
			Version:      3,
			CollectionID: "alpha",
			HeadingPath:  "H",
			Page:         4,
			Body:         texts[i],
			Vector:       cloneVec(vecs[i]),
		}
	}
	recs[n] = index.VectorRecord{
		ChunkID:      "empty",
		DocID:        99,
		CollectionID: "alpha",
		Body:         "",
		Vector:       cloneVec(vecs[n]),
	}
	if err := disk.UpsertRecords(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := disk.UpsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{{
		ChunkID:      "new",
		DocID:        100,
		CollectionID: "alpha",
		Body:         "crimson lantern",
		Vector:       embed.Vector{1, 0, 0},
	}}); err == nil {
		t.Fatal("short vector should fail")
	}

	again := openDisk(t, dir, meta)
	idx, err := again.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchKeyword(ctx, index.Query{
		CollectionID: "alpha",
		Tokens:       []string{"crimson"},
		TopK:         100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != n {
		t.Fatalf("keyword hits %d", len(hits))
	}
	for _, hit := range hits {
		if hit.Score <= 0 || hit.Chunk.ID == "empty" || hit.Chunk.ID == "new" || hit.CollectionID != "alpha" {
			t.Fatalf("%+v", hit)
		}
	}
	found := findChunk(hits, "c00")
	if found.ParentID != 100 || found.HeadingPath != "H" || found.Chunk.DocumentID != "1" || found.Chunk.Text != texts[0] {
		t.Fatalf("%+v", found)
	}
	if found.Chunk.StartByte != 0 || found.Chunk.EndByte != 0 {
		t.Fatalf("bytes %+v", found.Chunk)
	}

	vh, err := idx.SearchVector(ctx, index.Query{CollectionID: "alpha", Vector: cloneVec(vecs[7]), TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(vh) == 0 || vh[0].Chunk.ID != "c07" || math.Abs(vh[0].Score-1) > 1e-6 {
		t.Fatalf("%+v", vh)
	}
	for _, hit := range vh {
		if hit.Score <= 0 || hit.Chunk.ID == "empty" {
			t.Fatalf("%+v", hit)
		}
	}

	updated := recs[0]
	updated.Body = "violet harbor"
	updated.Vector = mustEmbed(t, emb, updated.Body)
	if err := again.UpsertRecords(ctx, []index.VectorRecord{updated}); err != nil {
		t.Fatal(err)
	}
	left, err := idx.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"crimson"}, TopK: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != n-1 || findChunk(left, "c00").Chunk.ID != "" {
		t.Fatalf("after update %d", len(left))
	}
	violet, err := idx.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"violet"}, TopK: 10})
	if err != nil || len(violet) != 1 || violet[0].Chunk.ID != "c00" || violet[0].Chunk.Text != "violet harbor" {
		t.Fatalf("%+v %v", violet, err)
	}
}

func TestDiskUpsertKeepsDocumentID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	disk := openDisk(t, dir, meta)
	idx, err := disk.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	chunk := model.Chunk{
		ID:           "alpha-lantern#0",
		DocumentID:   "alpha-lantern",
		CollectionID: "alpha",
		Ordinal:      2,
		Text:         "crimson lantern",
		StartByte:    4,
		EndByte:      19,
	}
	vec := mustEmbed(t, emb, chunk.Text)
	if err := idx.Upsert(ctx, []model.Chunk{chunk}, []embed.Vector{vec}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, []model.Chunk{chunk}, nil); err == nil {
		t.Fatal("length mismatch should fail")
	}
	hits, err := idx.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"lantern"}, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Chunk.DocumentID != "alpha-lantern" || hits[0].Chunk.StartByte != 4 || hits[0].Chunk.EndByte != 19 || hits[0].Chunk.Ordinal != 2 {
		t.Fatalf("%+v", hits)
	}
}

func TestDiskCollectionIsolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	disk := openDisk(t, dir, meta)
	vec := mustEmbed(t, emb, "crimson lantern")
	err := disk.UpsertRecords(ctx, []index.VectorRecord{
		{ChunkID: "alpha-1", DocID: 1, CollectionID: "alpha", Body: "crimson lantern", Vector: cloneVec(vec)},
		{ChunkID: "beta-1", DocID: 2, CollectionID: "beta", Body: "crimson lantern", Vector: cloneVec(vec)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Get("alpha"); ok {
		t.Fatal("upsert should not open the collection")
	}
	if _, ok := disk.Get("gamma"); ok {
		t.Fatal("gamma should stay closed")
	}
	alpha, err := disk.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Get("beta"); ok {
		t.Fatal("beta was not opened")
	}
	hits, err := alpha.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"crimson", "lantern"}, TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Chunk.ID != "alpha-1" || hits[0].CollectionID != "alpha" {
		t.Fatalf("%+v", hits)
	}
	if _, err := alpha.SearchVector(ctx, index.Query{CollectionID: "beta", Vector: vec, TopK: 5}); err == nil {
		t.Fatal("alpha index should reject beta")
	}
	beta, err := disk.Open(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	bh, err := beta.SearchVector(ctx, index.Query{CollectionID: "beta", Vector: vec, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(bh) != 1 || bh[0].Chunk.ID != "beta-1" || bh[0].CollectionID != "beta" {
		t.Fatalf("%+v", bh)
	}
}

func TestDiskDimMismatchRefusesSearch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta := embed.ModelMeta{Name: "fixture", Dim: 8}
	emb := embed.NewFixture(meta.Dim)
	disk := openDisk(t, dir, meta)
	vec := mustEmbed(t, emb, "crimson lantern")
	if err := disk.UpsertRecords(ctx, []index.VectorRecord{{
		ChunkID: "c0", DocID: 1, CollectionID: "alpha", Body: "crimson lantern", Vector: vec,
	}}); err != nil {
		t.Fatal(err)
	}
	idx, err := disk.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}

	reg, err := index.NewDisk(dir, embed.ModelMeta{Name: "fixture", Dim: 4})
	if reg != nil || !errors.Is(err, index.ErrModelMismatch) || !strings.Contains(err.Error(), "refusing to search old vectors") {
		t.Fatalf("reg %v err %v", reg, err)
	}
	renamed, err := index.NewDisk(dir, embed.ModelMeta{Name: "other", Dim: 8})
	if renamed != nil || !errors.Is(err, index.ErrModelMismatch) {
		t.Fatalf("reg %v err %v", renamed, err)
	}
	again := openDisk(t, dir, meta)
	kept, err := again.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := kept.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"crimson"}, TopK: 5})
	if err != nil || len(hits) != 1 {
		t.Fatalf("%+v %v", hits, err)
	}

	t.Run("header", func(t *testing.T) {
		root := t.TempDir()
		d := openDisk(t, root, meta)
		if err := d.UpsertRecords(ctx, []index.VectorRecord{{
			ChunkID: "c0", DocID: 1, CollectionID: "alpha", Body: "crimson lantern", Vector: cloneVec(vec),
		}}); err != nil {
			t.Fatal(err)
		}
		opened, err := d.Open(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "collections", "alpha", "records.bin")
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{16, 0, 0, 0}, 8); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := opened.SearchVector(ctx, index.Query{CollectionID: "alpha", Vector: vec, TopK: 5})
		if got != nil || !errors.Is(err, index.ErrModelMismatch) {
			t.Fatalf("hits %v err %v", got, err)
		}
	})

	t.Run("meta", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{\"model_name\":\"fixture\",\"dim\":16}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := idx.SearchKeyword(ctx, index.Query{CollectionID: "alpha", Tokens: []string{"crimson"}, TopK: 5})
		if got != nil || !errors.Is(err, index.ErrModelMismatch) {
			t.Fatalf("hits %v err %v", got, err)
		}
	})
}

func openDisk(t *testing.T, root string, meta embed.ModelMeta) *index.Disk {
	t.Helper()
	reg, err := index.NewDisk(root, meta)
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reg.(*index.Disk)
	if !ok || disk == nil {
		t.Fatalf("%T", reg)
	}
	return disk
}

func mustEmbed(t *testing.T, emb embed.Embedder, text string) embed.Vector {
	t.Helper()
	vecs, err := emb.Embed(context.Background(), []string{text})
	if err != nil || len(vecs) != 1 {
		t.Fatal(err)
	}
	return cloneVec(vecs[0])
}

func cloneVec(v embed.Vector) embed.Vector {
	out := make(embed.Vector, len(v))
	copy(out, v)
	return out
}

func findChunk(hits []index.Hit, id string) index.Hit {
	for _, hit := range hits {
		if string(hit.Chunk.ID) == id {
			return hit
		}
	}
	return index.Hit{}
}
