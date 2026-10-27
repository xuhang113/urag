package index

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
)

func TestMilvusRejectsEmptyAddr(t *testing.T) {
	calls := 0
	prev := dialMilvus
	dialMilvus = func(context.Context, string) (milvusSession, error) {
		calls++
		return nil, errors.New("dialed")
	}
	t.Cleanup(func() { dialMilvus = prev })

	writer, err := NewMilvusWriter("  ", embed.ModelMeta{Name: "fixture", Dim: 4})
	if writer != nil || err == nil || calls != 0 || !strings.Contains(err.Error(), "URAG_MILVUS_ADDR") {
		t.Fatalf("writer %v err %v calls %d", writer, err, calls)
	}
	reg, closer, err := NewMilvus("", embed.ModelMeta{Name: "fixture", Dim: 4}, NewMemory())
	if reg != nil || closer != nil || err == nil || calls != 0 {
		t.Fatalf("reg %v closer %v err %v calls %d", reg, closer, err, calls)
	}
}

func TestMilvusOneCollectionVectorAndKeyword(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMilvus{}
	const addr = "milvus.example.invalid:1"
	prev := dialMilvus
	dialMilvus = func(_ context.Context, got string) (milvusSession, error) {
		if got != addr {
			t.Fatalf("dialed %s", got)
		}
		return fake, nil
	}
	t.Cleanup(func() { dialMilvus = prev })

	meta := embed.ModelMeta{Name: "fixture", Dim: 4}
	writer, err := NewMilvusWriter(addr, meta)
	if err != nil {
		t.Fatal(err)
	}
	vec := embed.Vector{1, 0, 0, 0}
	err = writer.UpsertRecords(ctx, []VectorRecord{{
		ChunkID:      "c1",
		ParentID:     9,
		DocID:        3,
		Version:      2,
		CollectionID: "alpha",
		HeadingPath:  "H",
		Page:         4,
		Body:         "crimson lantern",
		Vector:       vec,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := writer.Get("alpha"); ok {
		t.Fatal("upsert opened the collection")
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "beta") || strings.Contains(call, "gamma") || strings.Contains(call, "keyword") || strings.Contains(call, "bm25") {
			t.Fatalf("call %s", call)
		}
	}
	rows := fake.rows["alpha"]
	if len(rows) != 1 || rows[0].Body != "crimson lantern" || rows[0].ParentID != 9 || rows[0].DocID != 3 || rows[0].Version != 2 || rows[0].HeadingPath != "H" || rows[0].Page != 4 {
		t.Fatalf("%+v", rows)
	}
	if len(rows[0].Vector) != 4 || strings.Contains(rows[0].Body, "%PDF") {
		t.Fatalf("%+v", rows[0])
	}

	keywords := NewMemory()
	err = WriteKeywords(ctx, keywords, []VectorRecord{
		{ChunkID: "c1", ParentID: 9, DocID: 3, CollectionID: "alpha", HeadingPath: "H", Body: "crimson lantern", StartByte: 1, EndByte: 4},
		{ChunkID: "c2", ParentID: 8, DocID: 3, CollectionID: "alpha", Body: "violet harbor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, closer, err := NewMilvus(addr, meta, keywords)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	idx, err := reg.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	before := countCalls(fake.calls, "search:")
	kw, err := idx.SearchKeyword(ctx, Query{
		CollectionID: "alpha",
		Tokens:       []string{"violet"},
		TopK:         5,
		Rank:         RankBM25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kw) != 1 || kw[0].Chunk.ID != "c2" || kw[0].CollectionID != "alpha" || kw[0].ParentID != 8 {
		t.Fatalf("%+v", kw)
	}
	if countCalls(fake.calls, "search:") != before {
		t.Fatalf("keyword search queried milvus: %v", fake.calls)
	}
	vh, err := idx.SearchVector(ctx, Query{CollectionID: "alpha", Vector: vec, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(vh) != 1 || vh[0].Chunk.ID != "c1" || vh[0].Chunk.Text != "crimson lantern" || vh[0].ParentID != 9 || vh[0].HeadingPath != "H" || vh[0].Chunk.DocumentID != "3" {
		t.Fatalf("%+v", vh)
	}
	if _, err := reg.Open(ctx, "beta"); err == nil {
		t.Fatal("beta should stay closed")
	}
	if _, ok := reg.Get("gamma"); ok {
		t.Fatal("gamma should stay closed")
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "create:beta") || strings.HasPrefix(call, "search:beta") {
			t.Fatalf("opened beta: %v", fake.calls)
		}
	}
	raw, err := writer.Open(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.SearchKeyword(ctx, Query{CollectionID: "alpha", Rank: RankBM25, TopK: 1}); err == nil {
		t.Fatal("milvus index should refuse keyword search")
	}

	other, err := NewMilvusWriter(addr, embed.ModelMeta{Name: "fixture", Dim: 8})
	if err != nil {
		t.Fatal(err)
	}
	err = other.UpsertRecords(ctx, []VectorRecord{{
		ChunkID: "c9", DocID: 1, CollectionID: "alpha", Body: "x", Vector: embed.Vector{1, 0, 0, 0, 0, 0, 0, 0},
	}})
	if !errors.Is(err, ErrModelMismatch) {
		t.Fatalf("%v", err)
	}
	if len(fake.rows["alpha"]) != 1 {
		t.Fatalf("mismatch wrote rows: %+v", fake.rows["alpha"])
	}
}

func TestMilvusSourceDoesNotListOrGuess(t *testing.T) {
	for _, name := range []string{"milvus.go", "milvus_sdk.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, frag := range []string{"ListCollections", "ShowCollections", "19530", "localhost"} {
			if strings.Contains(text, frag) {
				t.Fatalf("%s contains %s", name, frag)
			}
		}
	}
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

type fakeMilvus struct {
	dim    int
	exists map[string]bool
	rows   map[string][]milvusRow
	calls  []string
}

func (f *fakeMilvus) Close() error {
	f.calls = append(f.calls, "close")
	return nil
}

func (f *fakeMilvus) Has(_ context.Context, name string) (bool, error) {
	f.calls = append(f.calls, "has:"+name)
	return f.exists[name], nil
}

func (f *fakeMilvus) Create(_ context.Context, name string, dim int) error {
	f.calls = append(f.calls, "create:"+name)
	if f.dim == 0 {
		f.dim = dim
	}
	if dim != f.dim {
		return errors.New("dim")
	}
	if f.exists == nil {
		f.exists = map[string]bool{}
	}
	if f.rows == nil {
		f.rows = map[string][]milvusRow{}
	}
	f.exists[name] = true
	return nil
}

func (f *fakeMilvus) Dim(_ context.Context, name string) (int, error) {
	f.calls = append(f.calls, "dim:"+name)
	if !f.exists[name] {
		return 0, errors.New("missing")
	}
	return f.dim, nil
}

func (f *fakeMilvus) Upsert(_ context.Context, name string, rows []milvusRow) error {
	f.calls = append(f.calls, "upsert:"+name)
	if !f.exists[name] {
		return errors.New("missing")
	}
	pos := map[string]int{}
	cur := f.rows[name]
	for i, row := range cur {
		pos[row.ChunkID] = i
	}
	for _, row := range rows {
		copyVec := append([]float32(nil), row.Vector...)
		row.Vector = copyVec
		if i, ok := pos[row.ChunkID]; ok {
			cur[i] = row
			continue
		}
		pos[row.ChunkID] = len(cur)
		cur = append(cur, row)
	}
	f.rows[name] = cur
	return nil
}

func (f *fakeMilvus) Flush(_ context.Context, name string) error {
	f.calls = append(f.calls, "flush:"+name)
	return nil
}

func (f *fakeMilvus) Search(_ context.Context, name string, vec []float32, topK int) ([]milvusHit, error) {
	f.calls = append(f.calls, "search:"+name)
	if !f.exists[name] {
		return nil, errors.New("missing")
	}
	hits := make([]milvusHit, 0)
	for _, row := range f.rows[name] {
		score := cosine(embed.Vector(vec), embed.Vector(row.Vector))
		if score <= 0 {
			continue
		}
		hits = append(hits, milvusHit{Row: row, Score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Row.ChunkID < hits[j].Row.ChunkID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}
