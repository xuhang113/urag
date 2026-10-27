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
// 内存索引没有父块，ParentID 为 0，HeadingPath 为空。
// 磁盘记录把父块编号和标题路径带到命中上。
type Hit struct {
	Chunk        model.Chunk
	Score        float64
	CollectionID model.CollectionID
	ParentID     int64
	HeadingPath  string
}

// Query 是对一个集合的查询。CollectionID 必须点名。
type Query struct {
	CollectionID model.CollectionID
	Vector       embed.Vector
	Tokens       []string
	TopK         int
}

// Index 只服务 Collection 返回的那一个集合。
// 关键词分数固定为 |queryTokens ∩ chunkTokens| / |queryTokens|，查询无词时为 0。
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

	// SearchKeyword 在本集合内按词重叠分数搜索。
	// 分数是 |queryTokens ∩ chunkTokens| / |queryTokens|。查询无词时分数为 0。
	// q.CollectionID 必须等于本集合，否则返回错误。最多返回 TopK 条。
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
