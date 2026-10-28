package index

import (
	"context"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
)

func TestMilvusBodyFieldsOmitVector(t *testing.T) {
	fields := milvusBodyFields()
	if len(fields) == 0 {
		t.Fatal("no body fields")
	}
	for _, name := range fields {
		if name == fieldVector || name == "vector" {
			t.Fatalf("body query reads %s", name)
		}
	}
}

func TestCopyBodiesWritesCollectionIndex(t *testing.T) {
	ctx := context.Background()
	fakeM := &fakeMilvus{
		dim:    4,
		exists: map[string]bool{"public": true},
		rows: map[string][]milvusRow{
			"public": {{
				ChunkID: "c1", ParentID: 42, DocID: 7, Version: 1,
				CollectionID: "public", HeadingPath: "东桥", Body: "crimson lantern",
				Vector: []float32{1, 0, 0, 0},
			}},
		},
	}
	prevM := dialMilvus
	dialMilvus = func(context.Context, string) (milvusSession, error) { return fakeM, nil }
	t.Cleanup(func() { dialMilvus = prevM })

	prevIter := iterMilvusChunks
	iterMilvusChunks = func(ctx context.Context, session milvusSession, name string, fn func(milvusRow) error) error {
		fake := session.(*fakeMilvus)
		fake.calls = append(fake.calls, "each:"+name)
		for _, row := range fake.rows[name] {
			row.Vector = nil
			if err := fn(row); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	t.Cleanup(func() { iterMilvusChunks = prevIter })

	fakeE := &nameElastic{}
	prevE := dialElastic
	dialElastic = func(context.Context, string) (elasticSession, error) { return fakeE, nil }
	t.Cleanup(func() { dialElastic = prevE })

	writer, err := NewMilvusWriter("milvus.example.invalid:1", embed.ModelMeta{Name: "fixture", Dim: 4})
	if err != nil {
		t.Fatal(err)
	}
	es, err := NewElastic("http://elastic.example.invalid:9200")
	if err != nil {
		t.Fatal(err)
	}
	n, err := writer.CopyBodies(ctx, "public", es)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("written %d", n)
	}
	if !containsCall(fakeE.calls, "create:public") || !containsCall(fakeE.calls, "bulk:public:c1") {
		t.Fatalf("calls %v", fakeE.calls)
	}
	for _, call := range fakeM.calls {
		if strings.HasPrefix(call, "search:") {
			t.Fatalf("copy searched vectors: %s", call)
		}
	}
}
