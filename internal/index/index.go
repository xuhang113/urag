// Package index 定义按集合隔离的向量索引和关键词索引。
// 一个集合一个 Index。本包不提供搜索全部集合的方法，也不嵌入文本。
package index

import (
	"context"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

// Hit 是一个集合内的一条命中。
// CollectionID 与这条命中所属的集合相同。
// 只通过 Upsert 写入的内存片段没有父块，ParentID 为 0，HeadingPath 为空。
// WriteKeywords 和磁盘记录会把父块编号和标题路径带到命中上。
type Hit struct {
	Chunk        model.Chunk
	Score        float64
	CollectionID model.CollectionID
	ParentID     int64
	HeadingPath  string
}

// RankBM25 让 SearchKeyword 使用该集合关键词索引上的 BM25。
// 空字符串仍是词重叠比例，召回夹具继续用这个比例。
const RankBM25 = "bm25"

// Query 是对一个集合的查询。CollectionID 必须点名。
// Rank 为空时 SearchKeyword 使用词重叠比例。
// Rank 为 RankBM25 时，分数只来自本集合的关键词索引。
type Query struct {
	CollectionID model.CollectionID
	Vector       embed.Vector
	Tokens       []string
	TopK         int
	Rank         string
}

// Index 只服务 Collection 返回的那一个集合。
// Rank 为空时，关键词分数是 |queryTokens ∩ chunkTokens| / |queryTokens|，查询无词时为 0。
// Rank 为 RankBM25 时，关键词分数是本集合的 BM25。
// 向量分数是余弦相似度。分数小于等于 0 的命中不返回。
type Index interface {
	// Collection 返回这个索引所属的集合。
	Collection() model.CollectionID

	// Upsert 写入片段和对应向量。
	// chunks 与 vectors 必须等长，且每个片段的 CollectionID 等于本索引的集合。
	// 违反时返回错误，不写入。
	Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error

	// SearchVector 在本集合内按余弦相似度搜索。
	// q.CollectionID 必须等于本集合，否则返回错误。最多返回 TopK 条。
	SearchVector(ctx context.Context, q Query) ([]Hit, error)

	// SearchKeyword 在本集合的关键词索引里搜索。
	// Rank 为空时，分数是 |queryTokens ∩ chunkTokens| / |queryTokens|。
	// Rank 为 RankBM25 时，分数是本集合上的 BM25。查询无词时分数为 0。
	// q.CollectionID 必须等于本集合，否则返回错误。最多返回 TopK 条。
	// 不会把 BM25 写进向量库，也不会查到另一个集合。
	SearchKeyword(ctx context.Context, q Query) ([]Hit, error)
}

// Registry 按集合打开索引。
// 它没有搜索全部集合的方法。Get 只交出已经打开的索引。
type Registry interface {
	// Open 打开 id 这一个集合的索引。失败时不返回其他集合的索引。
	Open(ctx context.Context, id model.CollectionID) (Index, error)

	// Get 返回已打开的集合索引。未打开时 ok 为 false。
	Get(id model.CollectionID) (Index, bool)
}
