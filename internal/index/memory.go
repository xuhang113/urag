package index

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

type stored struct {
	chunk    model.Chunk
	vec      embed.Vector
	parentID int64
	heading  string
}

type memoryIndex struct {
	id    model.CollectionID
	mu    sync.RWMutex
	order []model.ChunkID
	items map[model.ChunkID]stored
}

type memoryRegistry struct {
	mu    sync.Mutex
	items map[model.CollectionID]*memoryIndex
}

// NewMemory 返回按集合分开的内存注册表。
// Open 为每个集合建立一份独立索引；再次打开同一个集合返回同一份索引。
// 没有搜索全部集合的方法。
func NewMemory() Registry {
	return &memoryRegistry{items: make(map[model.CollectionID]*memoryIndex)}
}

func (r *memoryRegistry) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = make(map[model.CollectionID]*memoryIndex)
	}
	if idx, ok := r.items[id]; ok {
		return idx, nil
	}
	idx := &memoryIndex{
		id:    id,
		items: make(map[model.ChunkID]stored),
	}
	r.items[id] = idx
	return idx, nil
}

func (r *memoryRegistry) Get(id model.CollectionID) (Index, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, ok := r.items[id]
	if !ok {
		return nil, false
	}
	return idx, true
}

func (m *memoryIndex) Collection() model.CollectionID { return m.id }

func (m *memoryIndex) Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("index: %d chunks and %d vectors", len(chunks), len(vectors))
	}
	for _, c := range chunks {
		if c.CollectionID != m.id {
			return fmt.Errorf("index: chunk %s belongs to %s", c.ID, c.CollectionID)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range chunks {
		vec := make(embed.Vector, len(vectors[i]))
		copy(vec, vectors[i])
		prev, exists := m.items[c.ID]
		if !exists {
			m.order = append(m.order, c.ID)
		}
		m.items[c.ID] = stored{
			chunk:    c,
			vec:      vec,
			parentID: prev.parentID,
			heading:  prev.heading,
		}
	}
	return nil
}

func (m *memoryIndex) SearchVector(ctx context.Context, q Query) ([]Hit, error) {
	if err := m.check(ctx, q); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.collect(func(item stored) float64 {
		return cosine(q.Vector, item.vec)
	}, q.TopK), nil
}

func (m *memoryIndex) SearchKeyword(ctx context.Context, q Query) ([]Hit, error) {
	if err := m.check(ctx, q); err != nil {
		return nil, err
	}
	if q.Rank != "" && q.Rank != RankBM25 {
		return nil, fmt.Errorf("index: unknown keyword rank %q", q.Rank)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if q.Rank == RankBM25 {
		return m.collectBM25(q.Tokens, q.TopK), nil
	}
	return m.collect(func(item stored) float64 {
		return keywordScore(q.Tokens, item.chunk.Text)
	}, q.TopK), nil
}

func (m *memoryIndex) check(ctx context.Context, q Query) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.CollectionID != m.id {
		return fmt.Errorf("index: collection %s is not %s", q.CollectionID, m.id)
	}
	return nil
}

func (m *memoryIndex) collect(scoreOf func(stored) float64, topK int) []Hit {
	if topK <= 0 {
		return []Hit{}
	}
	hits := make([]Hit, 0)
	for _, id := range m.order {
		item := m.items[id]
		score := scoreOf(item)
		if score <= 0 || math.IsNaN(score) {
			continue
		}
		hits = append(hits, Hit{
			Chunk:        item.chunk,
			Score:        score,
			CollectionID: m.id,
			ParentID:     item.parentID,
			HeadingPath:  item.heading,
		})
	}
	sortIndexHits(hits)
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

func (m *memoryIndex) collectBM25(query []string, topK int) []Hit {
	if topK <= 0 || len(uniqueTokens(query)) == 0 {
		return []Hit{}
	}
	docs := make([][]string, len(m.order))
	for i, id := range m.order {
		docs[i] = embed.Tokens(m.items[id].chunk.Text)
	}
	scores := bm25Scores(docs, query)
	hits := make([]Hit, 0)
	for i, id := range m.order {
		score := scores[i]
		if score <= 0 || math.IsNaN(score) {
			continue
		}
		item := m.items[id]
		hits = append(hits, Hit{
			Chunk:        item.chunk,
			Score:        score,
			CollectionID: m.id,
			ParentID:     item.parentID,
			HeadingPath:  item.heading,
		})
	}
	sortIndexHits(hits)
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

func sortIndexHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Chunk.ID < hits[j].Chunk.ID
		}
		return hits[i].Score > hits[j].Score
	})
}

// WriteKeywords 把子块正文写进这些记录自己的集合关键词索引。
// 不写向量库，也不把一个集合的正文写进另一个集合。
// 向量为空时保留该片段已有的向量。reg 必须是 NewMemory 的结果。
func WriteKeywords(ctx context.Context, reg Registry, recs []VectorRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mem, ok := reg.(*memoryRegistry)
	if !ok || mem == nil {
		return errors.New("index: keyword writes need the memory index")
	}
	return mem.writeKeywords(ctx, recs)
}

func (r *memoryRegistry) writeKeywords(ctx context.Context, recs []VectorRecord) error {
	grouped := make(map[model.CollectionID][]VectorRecord)
	order := make([]model.CollectionID, 0)
	for _, rec := range recs {
		if err := validCollection(rec.CollectionID); err != nil {
			return err
		}
		if rec.ChunkID == "" {
			return errors.New("index: empty chunk id")
		}
		if _, ok := grouped[rec.CollectionID]; !ok {
			order = append(order, rec.CollectionID)
		}
		grouped[rec.CollectionID] = append(grouped[rec.CollectionID], rec)
	}
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		idx, err := r.Open(ctx, id)
		if err != nil {
			return err
		}
		mem, ok := idx.(*memoryIndex)
		if !ok || mem.id != id {
			return fmt.Errorf("index: opened keyword collection %s", id)
		}
		if err := mem.putKeywords(grouped[id]); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryIndex) putKeywords(recs []VectorRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range recs {
		if rec.CollectionID != m.id {
			return fmt.Errorf("index: chunk %s belongs to %s", rec.ChunkID, rec.CollectionID)
		}
		if rec.ChunkID == "" {
			return errors.New("index: empty chunk id")
		}
		id := model.ChunkID(rec.ChunkID)
		prev, exists := m.items[id]
		vec := prev.vec
		if len(rec.Vector) > 0 {
			vec = append(embed.Vector(nil), rec.Vector...)
		}
		if !exists {
			m.order = append(m.order, id)
		}
		docID := strconv.FormatInt(rec.DocID, 10)
		m.items[id] = stored{
			chunk: model.Chunk{
				ID:           id,
				DocumentID:   model.DocumentID(docID),
				CollectionID: m.id,
				Text:         rec.Body,
				StartByte:    rec.StartByte,
				EndByte:      rec.EndByte,
			},
			vec:      vec,
			parentID: rec.ParentID,
			heading:  rec.HeadingPath,
		}
	}
	return nil
}

// keywordScore 是查询词集合与片段词集合的交集大小，除以查询词集合的大小。
// 词来自调用方传入的 queryTokens，以及 embed.Tokens 对片段正文的切分。查询无词时为 0。
func keywordScore(queryTokens []string, chunkText string) float64 {
	q := tokenSet(queryTokens)
	if len(q) == 0 {
		return 0
	}
	c := tokenSet(embed.Tokens(chunkText))
	n := 0
	for tok := range q {
		if _, ok := c[tok]; ok {
			n++
		}
	}
	return float64(n) / float64(len(q))
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		set[tok] = struct{}{}
	}
	return set
}

func cosine(a, b embed.Vector) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
