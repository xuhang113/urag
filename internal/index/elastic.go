package index

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

const elasticBulkBatch = 256

// dialElastic 在第一次写入或打开点名集合时建立会话。
// 测试替换它。空地址在此之前就返回，不会调用。
var dialElastic = dialElasticHTTP

// elasticSession 是 Elasticsearch 的 HTTP 会话。
// 实现留在本包，签名里没有厂商客户端类型。
type elasticSession interface {
	Close() error
	Exists(ctx context.Context, index string) (bool, error)
	Create(ctx context.Context, index string) error
	Bulk(ctx context.Context, index string, docs []elasticDoc) error
	Refresh(ctx context.Context, index string) error
	Search(ctx context.Context, index, collectionID, query string, topK int) ([]elasticHit, error)
}

// elasticDoc 是一个集合里的一条子块。正文是清洗后的子块，不是 PDF 或 DOCX。
// 没有向量字段。BM25 由 Elasticsearch 计算。
type elasticDoc struct {
	ChunkID      string `json:"chunk_id"`
	ParentID     int64  `json:"parent_id"`
	DocID        int64  `json:"doc_id"`
	CollectionID string `json:"collection_id"`
	HeadingPath  string `json:"heading_path"`
	Body         string `json:"body"`
}

type elasticHit struct {
	Doc   elasticDoc
	Score float64
}

// Elastic 是一个集合一份关键词索引的连接。
// 一个 urag 集合对应一个 Elasticsearch index。向量不写在这里。
// 导出的方法不使用厂商客户端类型。
type Elastic struct {
	addr string
	dial func(context.Context, string) (elasticSession, error)

	mu      sync.Mutex
	session elasticSession
	open    map[model.CollectionID]*elasticIndex
	dirty   map[string]struct{}
}

// NewElastic 准备该进程要读写的 Elasticsearch。
// 地址必须来自 URAG_ELASTIC_ADDR。空地址返回错误，不拨号，也不猜主机或端口。
// 地址必须带 http 或 https。假值只放在 .env.example。
func NewElastic(addr string) (*Elastic, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("index: URAG_ELASTIC_ADDR is empty, refusing to dial a default address")
	}
	if err := checkElasticAddr(addr); err != nil {
		return nil, err
	}
	return &Elastic{
		addr:  addr,
		dial:  dialElastic,
		open:  make(map[model.CollectionID]*elasticIndex),
		dirty: make(map[string]struct{}),
	}, nil
}

func checkElasticAddr(addr string) error {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("index: URAG_ELASTIC_ADDR must be an http or https URL")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("index: URAG_ELASTIC_ADDR must be an http or https URL")
	}
	return nil
}

// Close 刷新写过的 index 并关掉空闲连接。没拨过号时什么也不做。
func (e *Elastic) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	session := e.session
	dirty := e.dirty
	e.session = nil
	e.dirty = nil
	e.mu.Unlock()
	if session == nil {
		return nil
	}
	var first error
	for name := range dirty {
		if err := session.Refresh(context.Background(), name); err != nil && first == nil {
			first = e.fail(err)
		}
	}
	if err := session.Close(); err != nil && first == nil {
		first = e.fail(err)
	}
	return first
}

// UpsertRecords 只创建并写入记录里点名的集合。
// 每个集合一个 index。不写向量，也不打开没有出现的集合。
// 正文是子块文本。调用方不要把 PDF 或 DOCX 原文放进来。
func (e *Elastic) UpsertRecords(ctx context.Context, recs []VectorRecord) error {
	if e == nil {
		return errors.New("index: nil elasticsearch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	grouped := make(map[model.CollectionID][]elasticDoc)
	order := make([]model.CollectionID, 0)
	for _, rec := range recs {
		if _, err := elasticIndexName(rec.CollectionID); err != nil {
			return err
		}
		doc, err := elasticDocFromRecord(rec)
		if err != nil {
			return err
		}
		if _, ok := grouped[rec.CollectionID]; !ok {
			order = append(order, rec.CollectionID)
		}
		grouped[rec.CollectionID] = append(grouped[rec.CollectionID], doc)
	}
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		session, name, err := e.ensure(ctx, id, true)
		if err != nil {
			return err
		}
		docs := dedupeElasticDocs(grouped[id])
		for start := 0; start < len(docs); start += elasticBulkBatch {
			end := start + elasticBulkBatch
			if end > len(docs) {
				end = len(docs)
			}
			if err := session.Bulk(ctx, name, docs[start:end]); err != nil {
				return e.fail(err)
			}
			e.markDirty(name)
		}
	}
	return nil
}

func (e *Elastic) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if e == nil {
		return nil, errors.New("index: nil elasticsearch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if idx, ok := e.open[id]; ok {
		e.mu.Unlock()
		return idx, nil
	}
	e.mu.Unlock()

	_, name, err := e.ensure(ctx, id, false)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if idx, ok := e.open[id]; ok {
		return idx, nil
	}
	idx := &elasticIndex{id: id, name: name, owner: e}
	e.open[id] = idx
	return idx, nil
}

func (e *Elastic) Get(id model.CollectionID) (Index, bool) {
	if e == nil {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	idx, ok := e.open[id]
	if !ok {
		return nil, false
	}
	return idx, true
}

func (e *Elastic) ensure(ctx context.Context, id model.CollectionID, create bool) (elasticSession, string, error) {
	name, err := elasticIndexName(id)
	if err != nil {
		return nil, "", err
	}
	session, err := e.connection(ctx)
	if err != nil {
		return nil, "", err
	}
	exists, err := session.Exists(ctx, name)
	if err != nil {
		return nil, "", e.fail(err)
	}
	if !exists {
		if !create {
			return nil, "", fmt.Errorf("index: elasticsearch index %s does not exist", name)
		}
		if err := session.Create(ctx, name); err != nil {
			exists, herr := session.Exists(ctx, name)
			if herr != nil || !exists {
				return nil, "", e.fail(err)
			}
		}
	}
	return session, name, nil
}

func (e *Elastic) connection(ctx context.Context) (elasticSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil {
		return e.session, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dial := e.dial
	if dial == nil {
		dial = dialElastic
	}
	session, err := dial(ctx, e.addr)
	if err != nil {
		return nil, e.fail(err)
	}
	e.session = session
	return session, nil
}

func (e *Elastic) markDirty(name string) {
	e.mu.Lock()
	if e.dirty == nil {
		e.dirty = make(map[string]struct{})
	}
	e.dirty[name] = struct{}{}
	e.mu.Unlock()
}

func (e *Elastic) fail(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if e != nil && e.addr != "" {
		msg = strings.ReplaceAll(msg, e.addr, "elasticsearch")
	}
	return fmt.Errorf("index: elasticsearch: %s", msg)
}

func elasticDocFromRecord(rec VectorRecord) (elasticDoc, error) {
	if rec.ChunkID == "" {
		return elasticDoc{}, errors.New("index: empty chunk id")
	}
	if rec.CollectionID == "" {
		return elasticDoc{}, errors.New("index: empty collection")
	}
	return elasticDoc{
		ChunkID:      rec.ChunkID,
		ParentID:     rec.ParentID,
		DocID:        rec.DocID,
		CollectionID: string(rec.CollectionID),
		HeadingPath:  rec.HeadingPath,
		Body:         rec.Body,
	}, nil
}

func elasticIndexName(id model.CollectionID) (string, error) {
	name, err := milvusCollectionName(id)
	if err != nil {
		return "", err
	}
	if name != strings.ToLower(name) {
		return "", fmt.Errorf("index: collection %q must be lowercase for elasticsearch", id)
	}
	return "urag-" + name, nil
}

func dedupeElasticDocs(in []elasticDoc) []elasticDoc {
	pos := make(map[string]int, len(in))
	out := make([]elasticDoc, 0, len(in))
	for _, doc := range in {
		if i, ok := pos[doc.ChunkID]; ok {
			out[i] = doc
			continue
		}
		pos[doc.ChunkID] = len(out)
		out = append(out, doc)
	}
	return out
}

type elasticIndex struct {
	id    model.CollectionID
	name  string
	owner *Elastic
}

func (e *elasticIndex) Collection() model.CollectionID { return e.id }

func (e *elasticIndex) Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("index: %d chunks and %d vectors", len(chunks), len(vectors))
	}
	recs := make([]VectorRecord, len(chunks))
	for i, chunk := range chunks {
		if chunk.CollectionID != e.id {
			return fmt.Errorf("index: chunk %s belongs to %s", chunk.ID, chunk.CollectionID)
		}
		recs[i] = storedFromChunk(chunk, nil).rec
		recs[i].CollectionID = e.id
	}
	return e.owner.UpsertRecords(ctx, recs)
}

func (e *elasticIndex) SearchVector(context.Context, Query) ([]Hit, error) {
	return nil, errors.New("index: vectors stay on the vector store")
}

// SearchKeyword 只查这个集合的 Elasticsearch index。
// 分数是 Elasticsearch 的 BM25，不在进程内重算。Rank 必须是 RankBM25。
// Text 非空时把原始查询交给中文分析器。否则才把 Tokens 用空格拼起来。
func (e *elasticIndex) SearchKeyword(ctx context.Context, q Query) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.CollectionID != e.id {
		return nil, fmt.Errorf("index: collection %s is not %s", q.CollectionID, e.id)
	}
	if q.Rank != RankBM25 {
		if q.Rank == "" {
			return nil, errors.New("index: elasticsearch keyword index only serves BM25")
		}
		return nil, fmt.Errorf("index: unknown keyword rank %q", q.Rank)
	}
	if q.TopK <= 0 {
		return []Hit{}, nil
	}
	text := elasticQueryText(q)
	if text == "" {
		return []Hit{}, nil
	}
	session, _, err := e.owner.ensure(ctx, e.id, false)
	if err != nil {
		return nil, err
	}
	raw, err := session.Search(ctx, e.name, string(e.id), text, q.TopK)
	if err != nil {
		return nil, e.owner.fail(err)
	}
	hits := make([]Hit, 0, len(raw))
	for _, item := range raw {
		if item.Score <= 0 || item.Doc.CollectionID != string(e.id) {
			continue
		}
		hits = append(hits, Hit{
			Chunk: model.Chunk{
				ID:           model.ChunkID(item.Doc.ChunkID),
				DocumentID:   model.DocumentID(strconv.FormatInt(item.Doc.DocID, 10)),
				CollectionID: e.id,
				Text:         item.Doc.Body,
			},
			Score:        item.Score,
			CollectionID: e.id,
			ParentID:     item.Doc.ParentID,
			HeadingPath:  item.Doc.HeadingPath,
		})
	}
	sortIndexHits(hits)
	if len(hits) > q.TopK {
		hits = hits[:q.TopK]
	}
	return hits, nil
}

func elasticQueryText(q Query) string {
	if text := strings.TrimSpace(q.Text); text != "" {
		return text
	}
	return strings.Join(uniqueTokens(q.Tokens), " ")
}

var (
	_ Registry = (*Elastic)(nil)
	_ Index    = (*elasticIndex)(nil)
)
