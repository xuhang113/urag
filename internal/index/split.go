package index

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

// NewSplit 把一个集合的向量库和关键词索引导到同一次打开。
// SearchVector 只查向量库。SearchKeyword 只查关键词索引。
// 关键词不会写进向量库，也不会查到另一个集合。
// Open 只打开这一个集合。任一侧失败时不返回索引。
func NewSplit(vectors, keywords Registry) (Registry, error) {
	if vectors == nil || keywords == nil {
		return nil, errors.New("index: nil split registry")
	}
	return &splitRegistry{
		vectors:  vectors,
		keywords: keywords,
		open:     make(map[model.CollectionID]*splitIndex),
	}, nil
}

type splitRegistry struct {
	vectors  Registry
	keywords Registry

	mu   sync.Mutex
	open map[model.CollectionID]*splitIndex
}

type splitIndex struct {
	id       model.CollectionID
	vectors  Index
	keywords Index
}

func (s *splitRegistry) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if idx, ok := s.open[id]; ok {
		s.mu.Unlock()
		return idx, nil
	}
	s.mu.Unlock()

	vec, err := s.vectors.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	if vec.Collection() != id {
		return nil, fmt.Errorf("index: opened vector collection %s", vec.Collection())
	}
	kw, err := s.keywords.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	if kw.Collection() != id {
		return nil, fmt.Errorf("index: opened keyword collection %s", kw.Collection())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.open[id]; ok {
		return idx, nil
	}
	idx := &splitIndex{id: id, vectors: vec, keywords: kw}
	s.open[id] = idx
	return idx, nil
}

func (s *splitRegistry) Get(id model.CollectionID) (Index, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.open[id]
	if !ok {
		return nil, false
	}
	return idx, true
}

func (m *splitIndex) Collection() model.CollectionID { return m.id }

func (m *splitIndex) Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if err := m.vectors.Upsert(ctx, chunks, vectors); err != nil {
		return err
	}
	return m.keywords.Upsert(ctx, chunks, vectors)
}

func (m *splitIndex) SearchVector(ctx context.Context, q Query) ([]Hit, error) {
	if q.CollectionID != m.id {
		return nil, fmt.Errorf("index: collection %s is not %s", q.CollectionID, m.id)
	}
	return m.vectors.SearchVector(ctx, q)
}

func (m *splitIndex) SearchKeyword(ctx context.Context, q Query) ([]Hit, error) {
	if q.CollectionID != m.id {
		return nil, fmt.Errorf("index: collection %s is not %s", q.CollectionID, m.id)
	}
	return m.keywords.SearchKeyword(ctx, q)
}

// BM25Connected 表示关键词索引能做 BM25。
// 磁盘和内存没有这个方法，调用方把它们当成已连接。
// 没连上 Elasticsearch 的试查索引返回 false，页面不能把这一步写成 BM25 完成。
func (m *splitIndex) BM25Connected() bool {
	type linker interface {
		BM25Connected() bool
	}
	if link, ok := m.keywords.(linker); ok {
		return link.BM25Connected()
	}
	return true
}
