package job

import (
	"context"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
)

func TestWriteChildVectorsUsesPassageNotQuery(t *testing.T) {
	ctx := context.Background()
	emb := &textEmbedder{dim: 2}
	dst := &recWriter{}
	kids := []catalog.CollectionChild{
		{Child: catalog.Child{ChunkID: "7#0", ParentID: 3, DocID: 7, Ordinal: 0, HeadingPath: "章", Page: 2, Body: "子块正文", StartByte: 1, EndByte: 4}, Version: 5},
		{Child: catalog.Child{ChunkID: "7#1", ParentID: 3, DocID: 7, Ordinal: 1, Body: "下一块"}, Version: 5},
	}
	if err := WriteChildVectors(ctx, emb, dst, "alpha", kids, 1); err != nil {
		t.Fatal(err)
	}
	if len(emb.got) != 2 || len(dst.recs) != 2 {
		t.Fatalf("embed %d records %d", len(emb.got), len(dst.recs))
	}
	if emb.got[0][0] != chunk.Passage("章", "子块正文") || strings.Contains(emb.got[0][0], "Instruct:") {
		t.Fatalf("embed text %q", emb.got[0][0])
	}
	rec := dst.recs[0]
	if rec.CollectionID != "alpha" || rec.Body != "子块正文" || rec.ParentID != 3 || rec.DocID != 7 || rec.Version != 5 || rec.HeadingPath != "章" || rec.Page != 2 || rec.StartByte != 1 || rec.EndByte != 4 {
		t.Fatalf("%+v", rec)
	}
	if strings.Contains(rec.Body, "%PDF") || len(rec.Vector) != 2 {
		t.Fatalf("%+v", rec)
	}
	if err := WriteChildVectors(ctx, nil, dst, "alpha", nil, 1); err == nil {
		t.Fatal("nil embedder should fail")
	}
}

type textEmbedder struct {
	dim int
	got [][]string
}

func (e *textEmbedder) Dim() int { return e.dim }

func (e *textEmbedder) Embed(_ context.Context, texts []string) ([]embed.Vector, error) {
	e.got = append(e.got, append([]string(nil), texts...))
	out := make([]embed.Vector, len(texts))
	for i := range out {
		out[i] = embed.Vector{float32(i + 1), 0}
	}
	return out, nil
}

type recWriter struct {
	recs []index.VectorRecord
}

func (w *recWriter) UpsertRecords(_ context.Context, recs []index.VectorRecord) error {
	w.recs = append(w.recs, recs...)
	return nil
}
