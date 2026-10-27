// Package retrieve 定义在点名集合上的召回契约。
// 本包负责按集合分别召回、混合分数和可选重排。
// 本包不打开请求里没有点名的集合，也不清洗文档。
package retrieve

import (
	"context"
	"errors"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// ErrCollectionsRequired 表示请求没有点名任何集合。
var ErrCollectionsRequired = errors.New("retrieve: collections required")

var errNotImplemented = errors.New("retrieve: not implemented")

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
// reranker 可以为空；此时即使请求要求重排，接通后也只返回合并结果。
// 当前除了空集合检查，Retrieve 返回未实现错误。
func New(reg index.Registry, emb embed.Embedder, weights Weights, reranker Reranker) Service {
	if weights.Vector == 0 && weights.Keyword == 0 {
		weights.Vector = 0.7
		weights.Keyword = 0.3
	}
	return service{reg: reg, emb: emb, weights: weights, reranker: reranker}
}

func (service) Retrieve(_ context.Context, req Request) ([]Hit, error) {
	if len(req.Collections) == 0 {
		return nil, ErrCollectionsRequired
	}
	return nil, errNotImplemented
}

type overlapReranker struct{}

// NewOverlapReranker 返回可手算的重排器。
// 接通后分数等于词重叠数乘以 1000，再加上余弦。
// 当前 Rerank 返回未实现错误。
func NewOverlapReranker() Reranker { return overlapReranker{} }

func (overlapReranker) Rerank(context.Context, string, []Hit) ([]Hit, error) {
	return nil, errNotImplemented
}
