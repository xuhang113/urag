// Package retrieve 定义在点名集合上的召回契约。
// 本包负责按集合分别召回、混合分数和可选重排。
// 本包不打开请求里没有点名的集合，也不清洗文档。
package retrieve

import (
	"context"
	"errors"
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
// StartByte 与 EndByte 是原文 Document.Text 上的字节位置。
type Hit struct {
	CollectionID model.CollectionID `json:"collection_id"`
	DocumentID   model.DocumentID   `json:"document_id"`
	ChunkID      model.ChunkID      `json:"chunk_id"`
	Text         string             `json:"text"`
	Score        float64            `json:"score"`
	StartByte    int                `json:"start_byte"`
	EndByte      int                `json:"end_byte"`
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
	return service{reg: reg, emb: emb, weights: weights, reranker: reranker}
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

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score == merged[j].Score {
			if merged[i].CollectionID == merged[j].CollectionID {
				if merged[i].DocumentID == merged[j].DocumentID {
					return merged[i].ChunkID < merged[j].ChunkID
				}
				return merged[i].DocumentID < merged[j].DocumentID
			}
			return merged[i].CollectionID < merged[j].CollectionID
		}
		return merged[i].Score > merged[j].Score
	})

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
	add := func(chunk model.Chunk, cos, kw float64) {
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
		})
	}
	for _, h := range vecHits {
		add(h.Chunk, h.Score, kwByID[h.Chunk.ID])
	}
	for _, h := range kwHits {
		add(h.Chunk, 0, h.Score)
	}
	return out, nil
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
