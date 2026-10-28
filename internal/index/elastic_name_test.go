package index

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/model"
)

func TestElasticIndexUsesSmartCN(t *testing.T) {
	if !strings.Contains(elasticIndexBody, "smartcn") || !strings.Contains(elasticIndexBody, `"type": "BM25"`) {
		t.Fatal("chinese analyzer or BM25 missing")
	}
	if strings.Contains(elasticIndexBody, "ik_") {
		t.Fatal("ik analyzer")
	}
}

func TestCollectionPublicUsesIndexPublic(t *testing.T) {
	name, err := elasticIndexName("public")
	if err != nil || name != "public" {
		t.Fatalf("name %q err %v", name, err)
	}
	if strings.Contains(name, "urag-") {
		t.Fatalf("index name %s", name)
	}
}

func TestElasticUpsertNamesIndexAfterCollection(t *testing.T) {
	fake := &nameElastic{}
	prev := dialElastic
	dialElastic = func(ctx context.Context, addr string) (elasticSession, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if addr != "http://elastic.example.invalid:9200" {
			t.Fatalf("dialed %s", addr)
		}
		return fake, nil
	}
	t.Cleanup(func() { dialElastic = prev })

	es, err := NewElastic("http://elastic.example.invalid:9200")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = es.UpsertRecords(ctx, []VectorRecord{{
		ChunkID:      "c1",
		ParentID:     42,
		DocID:        7,
		CollectionID: "public",
		HeadingPath:  "东桥",
		Body:         "crimson lantern",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	if !containsCall(fake.calls, "create:public") || !containsCall(fake.calls, "bulk:public:c1") || !containsCall(fake.calls, "refresh:public") {
		t.Fatalf("calls %v", fake.calls)
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "urag-") {
			t.Fatalf("prefixed index %s", call)
		}
	}

	idx, err := es.Open(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchKeyword(ctx, Query{
		CollectionID: "public",
		Text:         "crimson",
		TopK:         5,
		Rank:         RankBM25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Chunk.ID != "c1" || hits[0].ParentID != 42 || hits[0].HeadingPath != "东桥" {
		t.Fatalf("hits %+v", hits)
	}
	if !containsCall(fake.calls, "search:public") {
		t.Fatalf("calls %v", fake.calls)
	}
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

type nameElastic struct {
	calls []string
	live  map[string]bool
}

func (f *nameElastic) Close() error { return nil }

func (f *nameElastic) Exists(_ context.Context, index string) (bool, error) {
	f.calls = append(f.calls, "exists:"+index)
	return f.live[index], nil
}

func (f *nameElastic) Create(_ context.Context, index string) error {
	f.calls = append(f.calls, "create:"+index)
	if f.live == nil {
		f.live = map[string]bool{}
	}
	f.live[index] = true
	return nil
}

func (f *nameElastic) Bulk(_ context.Context, index string, docs []elasticDoc) error {
	if len(docs) != 1 || docs[0].ChunkID != "c1" || docs[0].CollectionID != "public" || docs[0].Body == "" {
		return errors.New("unexpected bulk doc")
	}
	f.calls = append(f.calls, "bulk:"+index+":"+docs[0].ChunkID)
	return nil
}

func (f *nameElastic) Refresh(_ context.Context, index string) error {
	f.calls = append(f.calls, "refresh:"+index)
	return nil
}

func (f *nameElastic) Search(_ context.Context, index, collectionID, query string, topK int) ([]elasticHit, error) {
	f.calls = append(f.calls, "search:"+index)
	if index != "public" || collectionID != "public" || query == "" || topK <= 0 {
		return nil, errors.New("unexpected search")
	}
	return []elasticHit{{
		Doc: elasticDoc{
			ChunkID:      "c1",
			ParentID:     42,
			DocID:        7,
			CollectionID: "public",
			HeadingPath:  "东桥",
			Body:         "crimson lantern",
		},
		Score: 1.5,
	}}, nil
}

var _ elasticSession = (*nameElastic)(nil)

func TestElasticNameTestDoesNotDial(t *testing.T) {
	if _, err := NewElastic(""); err == nil {
		t.Fatal("empty address dialed")
	}
	var id model.CollectionID = "public"
	if id != "public" {
		t.Fatal(id)
	}
}
