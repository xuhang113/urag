package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

const milvusTextMax = 65535

// dialMilvus 在第一次打开或写入点名集合时拨号。
// 测试替换它。空地址在此之前就返回，不会调用。
var dialMilvus = dialMilvusSDK

// milvusSession 是向量库会话。实现留在本包，签名不出现厂商类型。
type milvusSession interface {
	Close() error
	Has(ctx context.Context, name string) (bool, error)
	Create(ctx context.Context, name string, dim int) error
	Dim(ctx context.Context, name string) (int, error)
	Upsert(ctx context.Context, name string, rows []milvusRow) error
	Flush(ctx context.Context, name string) error
	Search(ctx context.Context, name string, vec []float32, topK int) ([]milvusHit, error)
}

// milvusRow 是一个集合里的一条子块。正文是清洗后的子块，不是 PDF 或 DOCX。
type milvusRow struct {
	ChunkID      string
	ParentID     int64
	DocID        int64
	Version      int64
	CollectionID string
	HeadingPath  string
	Page         int64
	Body         string
	Vector       []float32
}

type milvusHit struct {
	Row   milvusRow
	Score float64
}

// Milvus 是一个向量库连接。一个 urag 集合对应一个 collection。
// 关键词不写在这里。字段不导出厂商客户端。
type Milvus struct {
	addr string
	meta embed.ModelMeta
	dial func(context.Context, string) (milvusSession, error)

	mu      sync.Mutex
	session milvusSession
	open    map[model.CollectionID]*milvusIndex
	dirty   map[string]struct{}
}

// NewMilvusWriter 准备写入点名集合的向量。
// 地址必须来自 URAG_MILVUS_ADDR。空地址返回错误，不拨号，也不猜一个端口。
// 假值只放在 .env.example。
func NewMilvusWriter(addr string, meta embed.ModelMeta) (*Milvus, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("index: URAG_MILVUS_ADDR is empty, refusing to dial a default address")
	}
	if meta.Dim <= 0 || meta.Name == "" {
		return nil, fmt.Errorf("index: model dimension %d", meta.Dim)
	}
	return &Milvus{
		addr:  addr,
		meta:  meta,
		dial:  dialMilvus,
		open:  make(map[model.CollectionID]*milvusIndex),
		dirty: make(map[string]struct{}),
	}, nil
}

// NewMilvus 返回检索用的注册表。向量走 Milvus，BM25 走 keywords 这一个集合的关键词索引。
// keywords 为空、或地址为空时返回错误，不拨号。
func NewMilvus(addr string, meta embed.ModelMeta, keywords Registry) (Registry, io.Closer, error) {
	writer, err := NewMilvusWriter(addr, meta)
	if err != nil {
		return nil, nil, err
	}
	reg, err := NewSplit(writer, keywords)
	if err != nil {
		_ = writer.Close()
		return nil, nil, err
	}
	return reg, writer, nil
}

// Close 把写过的集合刷盘并关掉连接。没拨过号时什么也不做。
func (m *Milvus) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	session := m.session
	dirty := m.dirty
	m.session = nil
	m.dirty = nil
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	var first error
	for name := range dirty {
		if err := session.Flush(context.Background(), name); err != nil && first == nil {
			first = m.fail(err)
		}
	}
	if err := session.Close(); err != nil && first == nil {
		first = m.fail(err)
	}
	return first
}

// UpsertRecords 只创建并写入记录里点名的集合。
// 每个集合一个 collection。不写关键词，也不打开没有出现的集合。
func (m *Milvus) UpsertRecords(ctx context.Context, recs []VectorRecord) error {
	if m == nil {
		return errors.New("index: nil milvus")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	grouped := make(map[model.CollectionID][]milvusRow)
	order := make([]model.CollectionID, 0)
	for _, rec := range recs {
		name, err := milvusCollectionName(rec.CollectionID)
		if err != nil {
			return err
		}
		row, err := m.rowFromRecord(rec, name)
		if err != nil {
			return err
		}
		if _, ok := grouped[rec.CollectionID]; !ok {
			order = append(order, rec.CollectionID)
		}
		grouped[rec.CollectionID] = append(grouped[rec.CollectionID], row)
	}
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		session, name, err := m.ensure(ctx, id, true)
		if err != nil {
			return err
		}
		rows := dedupeMilvusRows(grouped[id])
		if err := session.Upsert(ctx, name, rows); err != nil {
			return m.fail(err)
		}
		m.markDirty(name)
	}
	return nil
}

func (m *Milvus) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if m == nil {
		return nil, errors.New("index: nil milvus")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if idx, ok := m.open[id]; ok {
		m.mu.Unlock()
		return idx, nil
	}
	m.mu.Unlock()

	_, name, err := m.ensure(ctx, id, false)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx, ok := m.open[id]; ok {
		return idx, nil
	}
	idx := &milvusIndex{id: id, name: name, owner: m}
	m.open[id] = idx
	return idx, nil
}

func (m *Milvus) Get(id model.CollectionID) (Index, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.open[id]
	if !ok {
		return nil, false
	}
	return idx, true
}

func (m *Milvus) ensure(ctx context.Context, id model.CollectionID, create bool) (milvusSession, string, error) {
	name, err := milvusCollectionName(id)
	if err != nil {
		return nil, "", err
	}
	session, err := m.connection(ctx)
	if err != nil {
		return nil, "", err
	}
	has, err := session.Has(ctx, name)
	if err != nil {
		return nil, "", m.fail(err)
	}
	if !has {
		if !create {
			return nil, "", fmt.Errorf("index: milvus collection %s does not exist", name)
		}
		if err := session.Create(ctx, name, m.meta.Dim); err != nil {
			has, herr := session.Has(ctx, name)
			if herr != nil || !has {
				return nil, "", m.fail(err)
			}
		}
	}
	dim, err := session.Dim(ctx, name)
	if err != nil {
		return nil, "", m.fail(err)
	}
	if dim != m.meta.Dim {
		return nil, "", fmt.Errorf("%w: milvus dim %d, open dim %d, refusing to search old vectors", ErrModelMismatch, dim, m.meta.Dim)
	}
	return session, name, nil
}

func (m *Milvus) connection(ctx context.Context) (milvusSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		return m.session, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dial := m.dial
	if dial == nil {
		dial = dialMilvus
	}
	session, err := dial(ctx, m.addr)
	if err != nil {
		return nil, m.fail(err)
	}
	m.session = session
	return session, nil
}

func (m *Milvus) markDirty(name string) {
	m.mu.Lock()
	if m.dirty == nil {
		m.dirty = make(map[string]struct{})
	}
	m.dirty[name] = struct{}{}
	m.mu.Unlock()
}

func (m *Milvus) fail(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if m != nil && m.addr != "" {
		msg = strings.ReplaceAll(msg, m.addr, "milvus")
	}
	return fmt.Errorf("index: milvus: %s", msg)
}

func (m *Milvus) rowFromRecord(rec VectorRecord, name string) (milvusRow, error) {
	row := milvusRow{
		ChunkID:      rec.ChunkID,
		ParentID:     rec.ParentID,
		DocID:        rec.DocID,
		Version:      int64(rec.Version),
		CollectionID: name,
		HeadingPath:  rec.HeadingPath,
		Page:         int64(rec.Page),
		Body:         rec.Body,
		Vector:       append([]float32(nil), rec.Vector...),
	}
	if err := m.validateRow(row, name); err != nil {
		return milvusRow{}, err
	}
	return row, nil
}

func (m *Milvus) validateRow(row milvusRow, name string) error {
	if row.ChunkID == "" || len(row.ChunkID) > 512 {
		return fmt.Errorf("index: chunk id length %d", len(row.ChunkID))
	}
	if row.CollectionID != name {
		return fmt.Errorf("index: chunk %s belongs to %s", row.ChunkID, row.CollectionID)
	}
	if len(row.HeadingPath) > milvusTextMax || len(row.Body) > milvusTextMax {
		return fmt.Errorf("index: text length exceeds %d", milvusTextMax)
	}
	if len(row.Vector) != m.meta.Dim {
		return fmt.Errorf("index: vector length %d, want %d", len(row.Vector), m.meta.Dim)
	}
	return nil
}

type milvusIndex struct {
	id    model.CollectionID
	name  string
	owner *Milvus
}

func (m *milvusIndex) Collection() model.CollectionID { return m.id }

func (m *milvusIndex) Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("index: %d chunks and %d vectors", len(chunks), len(vectors))
	}
	recs := make([]VectorRecord, len(chunks))
	for i, chunk := range chunks {
		if chunk.CollectionID != m.id {
			return fmt.Errorf("index: chunk %s belongs to %s", chunk.ID, chunk.CollectionID)
		}
		recs[i] = storedFromChunk(chunk, vectors[i]).rec
		recs[i].Vector = append(embed.Vector(nil), vectors[i]...)
		recs[i].StartByte = chunk.StartByte
		recs[i].EndByte = chunk.EndByte
	}
	return m.owner.UpsertRecords(ctx, recs)
}

func (m *milvusIndex) SearchVector(ctx context.Context, q Query) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.CollectionID != m.id {
		return nil, fmt.Errorf("index: collection %s is not %s", q.CollectionID, m.id)
	}
	if q.TopK <= 0 {
		return []Hit{}, nil
	}
	if len(q.Vector) != m.owner.meta.Dim {
		return nil, fmt.Errorf("index: vector length %d, want %d", len(q.Vector), m.owner.meta.Dim)
	}
	session, _, err := m.owner.ensure(ctx, m.id, false)
	if err != nil {
		return nil, err
	}
	raw, err := session.Search(ctx, m.name, q.Vector, q.TopK)
	if err != nil {
		return nil, m.owner.fail(err)
	}
	hits := make([]Hit, 0, len(raw))
	for _, item := range raw {
		if item.Score <= 0 || item.Row.CollectionID != m.name {
			continue
		}
		hits = append(hits, Hit{
			Chunk: model.Chunk{
				ID:           model.ChunkID(item.Row.ChunkID),
				DocumentID:   model.DocumentID(fmt.Sprintf("%d", item.Row.DocID)),
				CollectionID: m.id,
				Text:         item.Row.Body,
			},
			Score:        item.Score,
			CollectionID: m.id,
			ParentID:     item.Row.ParentID,
			HeadingPath:  item.Row.HeadingPath,
		})
	}
	sortIndexHits(hits)
	if len(hits) > q.TopK {
		hits = hits[:q.TopK]
	}
	return hits, nil
}

// SearchKeyword 不查 Milvus。BM25 留在该集合自己的关键词索引。
func (m *milvusIndex) SearchKeyword(context.Context, Query) ([]Hit, error) {
	return nil, errors.New("index: BM25 stays on the collection keyword index")
}

func milvusCollectionName(id model.CollectionID) (string, error) {
	s := string(id)
	if s == "" || len(s) > 128 {
		return "", fmt.Errorf("index: invalid collection %q", id)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return "", fmt.Errorf("index: invalid collection %q", id)
		}
	}
	return s, nil
}

func dedupeMilvusRows(in []milvusRow) []milvusRow {
	pos := make(map[string]int, len(in))
	out := make([]milvusRow, 0, len(in))
	for _, row := range in {
		if i, ok := pos[row.ChunkID]; ok {
			out[i] = row
			continue
		}
		pos[row.ChunkID] = len(out)
		out = append(out, row)
	}
	return out
}

var (
	_ Registry = (*Milvus)(nil)
	_ Index    = (*milvusIndex)(nil)
)
