package clean

import (
	"context"
	"sync"

	"github.com/xuhang113/urag/internal/model"
)

// MemoryDeduper 在进程内按集合记住正文哈希。
// 不同集合的哈希互不影响。Find 未命中时返回空编号、false 和 nil。
type MemoryDeduper struct {
	mu sync.Mutex
	by map[model.CollectionID]map[string]model.DocumentID
}

// NewMemoryDeduper 返回空的集合内去重表。
func NewMemoryDeduper() *MemoryDeduper {
	return &MemoryDeduper{by: map[model.CollectionID]map[string]model.DocumentID{}}
}

// Find 只查 col 这个集合。未命中时返回空编号、false 和 nil。
func (m *MemoryDeduper) Find(_ context.Context, col model.CollectionID, contentHash string) (model.DocumentID, bool, error) {
	if m == nil {
		return "", false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.by[col][contentHash]
	return id, ok, nil
}

// Put 把 contentHash 绑定到 col 里的 id。已有不同绑定时返回错误，不覆盖。
func (m *MemoryDeduper) Put(_ context.Context, col model.CollectionID, contentHash string, id model.DocumentID) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[model.CollectionID]map[string]model.DocumentID{}
	}
	if _, ok := m.by[col]; !ok {
		m.by[col] = map[string]model.DocumentID{}
	}
	if old, ok := m.by[col][contentHash]; ok && old != id {
		return errBindingExists
	}
	m.by[col][contentHash] = id
	return nil
}
