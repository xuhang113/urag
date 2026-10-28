package index

import (
	"context"
	"errors"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
)

func TestCopyMissingDiskRecordsSkipsExistingChunkID(t *testing.T) {
	ctx := context.Background()
	const addr = "milvus.example.invalid:1"
	fake := &fakeMilvus{
		dim:    4,
		exists: map[string]bool{"public": true},
		rows: map[string][]milvusRow{
			"public": {
				{ChunkID: "keep", Version: 1, CollectionID: "public", Body: "old", Vector: []float32{1, 0, 0, 0}},
				{ChunkID: "newer-on-disk", Version: 1, CollectionID: "public", Body: "stale", Vector: []float32{0, 1, 0, 0}},
			},
		},
	}
	prev := dialMilvus
	dialMilvus = func(_ context.Context, got string) (milvusSession, error) {
		if got != addr {
			t.Fatalf("dialed %s", got)
		}
		return fake, nil
	}
	t.Cleanup(func() { dialMilvus = prev })

	meta := embed.ModelMeta{Name: "fixture", Dim: 4}
	reg, err := NewDisk(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	disk := reg.(*Disk)
	vecKeep := embed.Vector{9, 9, 9, 9}
	vecNew := embed.Vector{0, 0, 1, 0}
	err = disk.UpsertRecords(ctx, []VectorRecord{
		{ChunkID: "keep", Version: 4, CollectionID: "public", Body: "should-skip", Vector: vecKeep},
		{ChunkID: "newer-on-disk", Version: 9, CollectionID: "public", Body: "still-skip", Vector: embed.Vector{0, 0, 0, 1}},
		{ChunkID: "add", Version: 1, CollectionID: "public", Body: "from-disk", Vector: embed.Vector{1, 0, 0, 0}},
		{ChunkID: "add", Version: 2, CollectionID: "public", Body: "from-disk-last", Vector: vecNew},
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewMilvusWriter(addr, meta)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := CopyMissingDiskRecords(ctx, disk, writer, "public", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DiskUnique != 3 || stats.Written != 1 || stats.Skipped != 2 || stats.MilvusBefore != 2 {
		t.Fatalf("%+v", stats)
	}
	if stats.MilvusAfter != 3 {
		t.Fatalf("after %+v rows %+v", stats, fake.rows["public"])
	}
	var added milvusRow
	for _, row := range fake.rows["public"] {
		if row.ChunkID == "keep" && (row.Body != "old" || row.Vector[0] != 1) {
			t.Fatalf("existing chunk rewritten: %+v", row)
		}
		if row.ChunkID == "newer-on-disk" && row.Body != "stale" {
			t.Fatalf("newer disk version rewrote milvus: %+v", row)
		}
		if row.ChunkID == "add" {
			added = row
		}
	}
	if added.Body != "from-disk-last" || added.Version != 2 || len(added.Vector) != 4 || added.Vector[2] != 1 {
		t.Fatalf("added %+v", added)
	}
}

func TestDisconnectedBM25DoesNotScore(t *testing.T) {
	ctx := context.Background()
	reg := NewDisconnectedBM25()
	idx, err := reg.Open(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if connected, ok := idx.(interface{ BM25Connected() bool }); !ok || connected.BM25Connected() {
		t.Fatal("disconnected index reported BM25")
	}
	_, err = idx.SearchKeyword(ctx, Query{CollectionID: "public", Rank: RankBM25, TopK: 5, Text: "网络"})
	if !errors.Is(err, ErrBM25Unavailable) {
		t.Fatal(err)
	}
	split, err := NewSplit(NewMemory(), reg)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := split.Open(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if connected, ok := opened.(interface{ BM25Connected() bool }); !ok || connected.BM25Connected() {
		t.Fatal("split reported BM25")
	}
}

func TestElasticReadyEmptyAddr(t *testing.T) {
	up, err := ElasticReady(context.Background(), "  ")
	if err != nil || up {
		t.Fatalf("up %v err %v", up, err)
	}
	if _, err := ElasticReady(context.Background(), "milvus.example.invalid:1"); err == nil {
		t.Fatal("bare host was accepted")
	}
}
