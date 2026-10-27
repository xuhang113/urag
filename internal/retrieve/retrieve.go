// Package retrieve 定义在点名集合上的召回契约。
// 本包负责按集合分别召回、混合分数和可选重排。
// 本包不打开请求里没有点名的集合，也不清洗文档。
package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// ErrCollectionsRequired 表示请求没有点名任何集合。
var ErrCollectionsRequired = errors.New("retrieve: collections required")

// Weights 是混合分数的权重。两者都为 0 时，New 采用 Vector 0.7、Keyword 0.3。
type Weights struct {
	Vector  float64 // 默认 0.7
	Keyword float64 // 默认 0.3
}

// Request 是一次召回。只搜索 Collections 里点名的集合。
type Request struct {
	Collections []model.CollectionID `json:"collections"`
	Query       string               `json:"query"`
	TopK        int                  `json:"top_k"`
	Hybrid      bool                 `json:"hybrid"`
	Rerank      bool                 `json:"rerank"`
}

// Hit 是合并后的一条命中。每条都带 CollectionID。
// StartByte 与 EndByte 是原文 Document.Text 上的字节位置。磁盘子块没有字节区间时为 0。
// ParentID 与 HeadingPath 的 JSON 字段是 parent_id、heading_path。
type Hit struct {
	CollectionID model.CollectionID `json:"collection_id"`
	DocumentID   model.DocumentID   `json:"document_id"`
	ChunkID      model.ChunkID      `json:"chunk_id"`
	Text         string             `json:"text"`
	Score        float64            `json:"score"`
	StartByte    int                `json:"start_byte"`
	EndByte      int                `json:"end_byte"`
	ParentID     int64              `json:"parent_id"`
	HeadingPath  string             `json:"heading_path"`
	// ChildText 是子块正文。父块写入 Text 之后，这里仍保留子块。JSON 字段是 chunk。
	ChildText string `json:"chunk,omitempty"`
	// ParentText 是父块正文。查不到时为空，Text 保持子块。JSON 字段是 parent。
	ParentText string `json:"parent,omitempty"`
}

const (
	// childCandidateK 是向量检索和 BM25 各自的 topK。
	// 合并之后才截到 childRecallLimit，避免向量分数打平时把更强的 BM25 命中挤出这 50 条。
	childCandidateK  = 200
	childRecallLimit = 50
	childKeepLimit   = 8
)

// ParentLookup 按父块编号取出整节正文。
type ParentLookup interface {
	GetParent(ctx context.Context, parentID int64) (body string, err error)
}

// Reranker 对已经合并的命中重新排序。
// 失败时返回错误，不修改调用方传入的切片。
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error)
}

// Service 执行点名集合上的召回。
type Service interface {
	// Retrieve 搜索 req.Collections。
	// 集合列表为空时返回 ErrCollectionsRequired。
	// 只搜索点名的集合，各自召回后再按分数合并。未点名的集合不会被打开。
	// Hybrid 时分数等于 Vector 权重乘以余弦，加上 Keyword 权重乘以重叠。
	// Rerank 为 true 且构造时传入了非空 Reranker 时，对合并结果重排，再截断到 TopK。
	// 每条 Hit 都带 CollectionID。
	Retrieve(ctx context.Context, req Request) ([]Hit, error)

	// RetrieveChildren 只打开 req.Collections。
	// 每个集合先取向量 top 200 和 BM25 top 200，合并后再留下前 50 个子块，只把这 50 对送给 rerank。
	// 查询侧使用 embed.QueryText，子块嵌入文本是标题路径和正文。
	// 重排后留下最多 8 条。parents 为空或查不到时，Text 保持子块正文。
	RetrieveChildren(ctx context.Context, req Request, rerank PassageReranker, parents ParentLookup) ([]Hit, error)
}

type service struct {
	reg      index.Registry
	emb      embed.Embedder
	weights  Weights
	reranker Reranker
}

// New 组装召回服务。
// weights 的 Vector 与 Keyword 都为 0 时，改用 0.7 和 0.3。
// reranker 可以为空；此时即使请求要求重排，也只返回合并结果。
func New(reg index.Registry, emb embed.Embedder, weights Weights, reranker Reranker) Service {
	if weights.Vector == 0 && weights.Keyword == 0 {
		weights.Vector = 0.7
		weights.Keyword = 0.3
	}
	return &service{reg: reg, emb: emb, weights: weights, reranker: reranker}
}

func (s service) Retrieve(ctx context.Context, req Request) ([]Hit, error) {
	if len(req.Collections) == 0 {
		return nil, ErrCollectionsRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.reg == nil {
		return nil, errors.New("retrieve: nil registry")
	}
	if s.emb == nil {
		return nil, errors.New("retrieve: nil embedder")
	}

	vecs, err := s.emb.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("retrieve: embedder returned unexpected count")
	}
	qtokens := embed.Tokens(req.Query)

	merged := make([]Hit, 0)
	for _, id := range req.Collections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idx, err := s.reg.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		if idx.Collection() != id {
			return nil, errors.New("retrieve: opened a different collection")
		}
		hits, err := s.searchOne(ctx, idx, index.Query{
			CollectionID: id,
			Vector:       vecs[0],
			Tokens:       qtokens,
			TopK:         math.MaxInt,
		}, req.Hybrid)
		if err != nil {
			return nil, err
		}
		merged = append(merged, hits...)
	}

	sortHits(merged)

	if req.Rerank && s.reranker != nil {
		reranked, err := s.reranker.Rerank(ctx, req.Query, merged)
		if err != nil {
			return nil, err
		}
		merged = reranked
	}
	if req.TopK <= 0 {
		return []Hit{}, nil
	}
	if len(merged) > req.TopK {
		merged = merged[:req.TopK]
	}
	if merged == nil {
		return []Hit{}, nil
	}
	return merged, nil
}

func (s service) searchOne(ctx context.Context, idx index.Index, q index.Query, hybrid bool) ([]Hit, error) {
	vecHits, err := idx.SearchVector(ctx, q)
	if err != nil {
		return nil, err
	}
	var kwHits []index.Hit
	if hybrid {
		kwHits, err = idx.SearchKeyword(ctx, q)
		if err != nil {
			return nil, err
		}
	}
	kwByID := make(map[model.ChunkID]float64, len(kwHits))
	for _, h := range kwHits {
		kwByID[h.Chunk.ID] = h.Score
	}

	seen := make(map[model.ChunkID]struct{})
	out := make([]Hit, 0)
	add := func(h index.Hit, cos, kw float64) {
		chunk := h.Chunk
		if _, ok := seen[chunk.ID]; ok {
			return
		}
		seen[chunk.ID] = struct{}{}
		score := cos
		if hybrid {
			score = s.weights.Vector*cos + s.weights.Keyword*kw
		}
		if score <= 0 || math.IsNaN(score) {
			return
		}
		out = append(out, Hit{
			CollectionID: idx.Collection(),
			DocumentID:   chunk.DocumentID,
			ChunkID:      chunk.ID,
			Text:         chunk.Text,
			Score:        score,
			StartByte:    chunk.StartByte,
			EndByte:      chunk.EndByte,
			ParentID:     h.ParentID,
			HeadingPath:  h.HeadingPath,
		})
	}
	for _, h := range vecHits {
		add(h, h.Score, kwByID[h.Chunk.ID])
	}
	for _, h := range kwHits {
		add(h, 0, h.Score)
	}
	return out, nil
}

func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			if hits[i].CollectionID == hits[j].CollectionID {
				if hits[i].DocumentID == hits[j].DocumentID {
					return hits[i].ChunkID < hits[j].ChunkID
				}
				return hits[i].DocumentID < hits[j].DocumentID
			}
			return hits[i].CollectionID < hits[j].CollectionID
		}
		return hits[i].Score > hits[j].Score
	})
}

// RetrieveChildren 在每个点名集合里做向量 top 200 和 BM25 top 200，合并后只把前 50 个子块送去重排，再留下最多 8 条。
// 查询向量和重排查询都是 embed.QueryText(问题)。文档侧不加指令。
// 子块嵌入文本是 heading_path + "\n" + body；标题路径为空时只有 body。
// 重排只产出分数。parents 为 nil，或父块查不到时，Hit.Text 保持子块 body。
// 未点名的集合不会被打开。不使用服务里原来的 Reranker。
// 调用方要在嵌入结束并关掉嵌入模型之后，再让重排模型开始打分。
func (s *service) RetrieveChildren(ctx context.Context, req Request, rerank PassageReranker, parents ParentLookup) ([]Hit, error) {
	if s == nil {
		return nil, errors.New("retrieve: nil service")
	}
	if len(req.Collections) == 0 {
		return nil, ErrCollectionsRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.reg == nil {
		return nil, errors.New("retrieve: nil registry")
	}
	if s.emb == nil {
		return nil, errors.New("retrieve: nil embedder")
	}
	if rerank == nil {
		return nil, errors.New("retrieve: nil reranker")
	}

	queryText := embed.QueryText(req.Query)
	vecs, err := s.emb.Embed(ctx, []string{queryText})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("retrieve: embedder returned unexpected count")
	}
	qtokens := embed.Tokens(req.Query)

	merged := make([]Hit, 0)
	for _, id := range req.Collections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idx, err := s.reg.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		if idx.Collection() != id {
			return nil, errors.New("retrieve: opened a different collection")
		}
		q := index.Query{
			CollectionID: id,
			Vector:       vecs[0],
			Tokens:       qtokens,
			TopK:         childCandidateK,
			Rank:         index.RankBM25,
		}
		vecHits, err := idx.SearchVector(ctx, q)
		if err != nil {
			return nil, err
		}
		kwHits, err := idx.SearchKeyword(ctx, q)
		if err != nil {
			return nil, err
		}
		hits, err := mergeChildHits(id, vecHits, kwHits, s.weights)
		if err != nil {
			return nil, err
		}
		merged = append(merged, hits...)
	}
	sortHits(merged)
	if len(merged) > childRecallLimit {
		merged = merged[:childRecallLimit]
	}

	passages := make([]string, len(merged))
	for i := range merged {
		passages[i] = childEmbedText(merged[i].HeadingPath, merged[i].Text)
	}
	scores, err := rerank.Score(ctx, queryText, passages)
	if err != nil {
		return nil, err
	}
	if len(scores) != len(passages) {
		return nil, fmt.Errorf("retrieve: reranker returned %d scores for %d passages", len(scores), len(passages))
	}
	for i, score := range scores {
		if math.IsNaN(score) {
			return nil, errors.New("retrieve: reranker returned NaN")
		}
		merged[i].Score = score
	}
	sortHits(merged)
	if len(merged) > childKeepLimit {
		merged = merged[:childKeepLimit]
	}
	if merged == nil {
		merged = []Hit{}
	}
	return applyParents(ctx, merged, parents)
}

func mergeChildHits(col model.CollectionID, vecHits, kwHits []index.Hit, w Weights) ([]Hit, error) {
	type acc struct {
		hit Hit
		vec float64
		kw  float64
	}
	order := make([]model.ChunkID, 0, len(vecHits)+len(kwHits))
	byID := make(map[model.ChunkID]*acc, len(vecHits)+len(kwHits))
	add := func(h index.Hit, vec, kw float64) error {
		if h.Chunk.CollectionID != "" && h.Chunk.CollectionID != col {
			return fmt.Errorf("retrieve: index returned collection %s for %s", h.Chunk.CollectionID, col)
		}
		if h.CollectionID != "" && h.CollectionID != col {
			return fmt.Errorf("retrieve: index returned collection %s for %s", h.CollectionID, col)
		}
		id := h.Chunk.ID
		item, ok := byID[id]
		if !ok {
			item = &acc{hit: Hit{
				CollectionID: col,
				DocumentID:   h.Chunk.DocumentID,
				ChunkID:      id,
				Text:         h.Chunk.Text,
				StartByte:    h.Chunk.StartByte,
				EndByte:      h.Chunk.EndByte,
				ParentID:     h.ParentID,
				HeadingPath:  h.HeadingPath,
				ChildText:    h.Chunk.Text,
			}}
			byID[id] = item
			order = append(order, id)
		} else {
			if item.hit.Text == "" {
				item.hit.Text = h.Chunk.Text
			}
			if item.hit.ChildText == "" {
				item.hit.ChildText = h.Chunk.Text
			}
			if item.hit.DocumentID == "" {
				item.hit.DocumentID = h.Chunk.DocumentID
			}
			if item.hit.StartByte == 0 && item.hit.EndByte == 0 && (h.Chunk.StartByte != 0 || h.Chunk.EndByte != 0) {
				item.hit.StartByte = h.Chunk.StartByte
				item.hit.EndByte = h.Chunk.EndByte
			}
			if item.hit.ParentID == 0 {
				item.hit.ParentID = h.ParentID
			}
			if item.hit.HeadingPath == "" {
				item.hit.HeadingPath = h.HeadingPath
			}
		}
		if vec > item.vec {
			item.vec = vec
		}
		if kw > item.kw {
			item.kw = kw
		}
		return nil
	}
	for _, h := range vecHits {
		if err := add(h, h.Score, 0); err != nil {
			return nil, err
		}
	}
	for _, h := range kwHits {
		if err := add(h, 0, h.Score); err != nil {
			return nil, err
		}
	}
	out := make([]Hit, 0, len(order))
	for _, id := range order {
		item := byID[id]
		item.hit.Score = w.Vector*item.vec + w.Keyword*item.kw
		if item.hit.Score <= 0 || math.IsNaN(item.hit.Score) {
			continue
		}
		out = append(out, item.hit)
	}
	return out, nil
}

func childEmbedText(heading, body string) string {
	if heading == "" {
		return body
	}
	return heading + "\n" + body
}

func applyParents(ctx context.Context, hits []Hit, parents ParentLookup) ([]Hit, error) {
	if parents == nil {
		return hits, nil
	}
	for i := range hits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hits[i].ChildText == "" {
			hits[i].ChildText = hits[i].Text
		}
		body, err := parents.GetParent(ctx, hits[i].ParentID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if body == "" {
			continue
		}
		hits[i].ParentText = body
		hits[i].Text = body
	}
	return hits, nil
}

// ReleaseOnEmbed 在 Embed 返回前调用 release，并且只调用一次。
// 调用方用它关掉嵌入进程，然后再加载重排模型。
func ReleaseOnEmbed(inner embed.Embedder, release func() error) embed.Embedder {
	return &releasingEmbedder{inner: inner, release: release}
}

type releasingEmbedder struct {
	inner   embed.Embedder
	release func() error
}

func (r *releasingEmbedder) Dim() int {
	if r == nil || r.inner == nil {
		return 0
	}
	return r.inner.Dim()
}

func (r *releasingEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	defer r.finish()
	if r == nil || r.inner == nil {
		return nil, errors.New("retrieve: nil embedder")
	}
	return r.inner.Embed(ctx, texts)
}

func (r *releasingEmbedder) finish() {
	if r == nil || r.release == nil {
		return
	}
	release := r.release
	r.release = nil
	_ = release()
}

type overlapReranker struct{}

// NewOverlapReranker 返回可手算的重排器。
// 分数等于词重叠个数乘以 1000，再加上查询与命中正文的余弦。
// 词重叠个数是两边词集合的交集大小，词由 embed.Tokens 切出。
// 余弦用 64 维夹具嵌入计算。单个词完全匹配时余弦为 1，分数为 1001。
// 不修改调用方传入的切片。
func NewOverlapReranker() Reranker { return overlapReranker{} }

func (overlapReranker) Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, len(hits))
	copy(out, hits)
	if len(out) == 0 {
		return []Hit{}, nil
	}

	texts := make([]string, len(out)+1)
	texts[0] = query
	for i := range out {
		texts[i+1] = out[i].Text
	}
	vecs, err := embed.NewFixture(0).Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(texts) {
		return nil, errors.New("retrieve: rerank embedder returned unexpected count")
	}
	qTokens := embed.Tokens(query)
	for i := range out {
		n := overlapCount(qTokens, embed.Tokens(out[i].Text))
		out[i].Score = float64(n)*1000 + cosine(vecs[0], vecs[i+1])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ChunkID < out[j].ChunkID
		}
		return out[i].Score > out[j].Score
	})
	return out, nil
}

func overlapCount(query, chunk []string) int {
	q := tokenSet(query)
	c := tokenSet(chunk)
	n := 0
	for tok := range q {
		if _, ok := c[tok]; ok {
			n++
		}
	}
	return n
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
