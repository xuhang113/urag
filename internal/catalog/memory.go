package catalog

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

type memoryStore struct {
	mu           sync.Mutex
	nextDoc      int64
	nextParent   int64
	docs         map[int64]Document
	bySource     map[string]int64
	parents      map[int64]Parent
	parentsByDoc map[int64][]int64
	children     map[int64][]Child
}

var _ Store = (*memoryStore)(nil)

// NewMemory 返回不连接数据库的台账。编号从 1 起，只增不复用。
func NewMemory() Store { return newMemory() }

func newMemory() *memoryStore {
	return &memoryStore{
		docs:         make(map[int64]Document),
		bySource:     make(map[string]int64),
		parents:      make(map[int64]Parent),
		parentsByDoc: make(map[int64][]int64),
		children:     make(map[int64][]Child),
	}
}

func (s *memoryStore) PutDocument(ctx context.Context, in Document) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateDocument(in); err != nil {
		return 0, err
	}
	in.FetchedAt = normalizeFetched(in.FetchedAt)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key := sourceKey(in.CollectionID, in.SourceURI)
	if id, ok := s.bySource[key]; ok {
		doc := s.docs[id]
		if doc.Checksum == in.Checksum {
			return id, nil
		}
		if doc.Version >= maxVersion {
			return 0, errVersionOverflow
		}
		doc.Version++
		doc.Checksum = in.Checksum
		doc.MediaType = in.MediaType
		doc.License = in.License
		doc.Title = in.Title
		doc.FetchedAt = in.FetchedAt
		s.docs[id] = doc
		return id, nil
	}
	s.nextDoc++
	in.DocID = s.nextDoc
	s.docs[in.DocID] = in
	s.bySource[key] = in.DocID
	return in.DocID, nil
}

func (s *memoryStore) PutParent(ctx context.Context, in Parent) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateParent(in); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, ok := s.docs[in.DocID]; !ok {
		return 0, ErrNotFound
	}
	s.nextParent++
	in.ParentID = s.nextParent
	s.parents[in.ParentID] = in
	s.parentsByDoc[in.DocID] = append(s.parentsByDoc[in.DocID], in.ParentID)
	return in.ParentID, nil
}

func (s *memoryStore) PutChild(ctx context.Context, in Child) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateChild(in); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, ok := s.parents[in.ParentID]
	if !ok || parent.DocID != in.DocID {
		return ErrNotFound
	}
	in.ChunkID = ChunkID(in.DocID, in.Ordinal)
	for _, kid := range s.children[in.DocID] {
		if kid.Ordinal == in.Ordinal {
			return ErrConflict
		}
	}
	s.children[in.DocID] = append(s.children[in.DocID], in)
	return nil
}

func (s *memoryStore) GetParent(ctx context.Context, parentID int64) (Parent, error) {
	if err := ctx.Err(); err != nil {
		return Parent{}, err
	}
	if parentID <= 0 {
		return Parent{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Parent{}, err
	}
	parent, ok := s.parents[parentID]
	if !ok {
		return Parent{}, ErrNotFound
	}
	return parent, nil
}

func (s *memoryStore) ListChildren(ctx context.Context, docID int64) ([]Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if docID <= 0 {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := s.docs[docID]; !ok {
		return nil, ErrNotFound
	}
	kids := s.children[docID]
	out := make([]Child, len(kids))
	copy(out, kids)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out, nil
}

func (s *memoryStore) ListCollectionChildren(ctx context.Context, collectionID string) ([]CollectionChild, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(collectionID) == "" {
		return nil, errors.New("catalog: empty collection_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]CollectionChild, 0)
	for id, doc := range s.docs {
		if doc.CollectionID != collectionID {
			continue
		}
		for _, child := range s.children[id] {
			out = append(out, CollectionChild{Child: child, Version: doc.Version})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out, nil
}

func (s *memoryStore) CountIndexed(ctx context.Context, collectionID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, doc := range s.docs {
		if doc.CollectionID == collectionID {
			n++
		}
	}
	return n, nil
}

func (s *memoryStore) ReplaceSections(ctx context.Context, docID int64, groups []ParentWithChildren) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBatch(docID, groups); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := s.docs[docID]; !ok {
		return ErrNotFound
	}
	for _, id := range s.parentsByDoc[docID] {
		delete(s.parents, id)
	}
	delete(s.parentsByDoc, docID)
	delete(s.children, docID)

	ids := make([]int64, 0, len(groups))
	kids := make([]Child, 0)
	for _, group := range groups {
		s.nextParent++
		parent := group.Parent
		parent.ParentID = s.nextParent
		parent.DocID = docID
		s.parents[parent.ParentID] = parent
		ids = append(ids, parent.ParentID)
		for _, child := range group.Children {
			child.ParentID = parent.ParentID
			child.DocID = docID
			child.ChunkID = ChunkID(docID, child.Ordinal)
			kids = append(kids, child)
		}
	}
	if len(ids) > 0 {
		s.parentsByDoc[docID] = ids
	}
	if len(kids) > 0 {
		s.children[docID] = kids
	}
	return nil
}
