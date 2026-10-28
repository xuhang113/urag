package index

import (
	"context"
	"errors"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

// ErrBM25Unavailable 表示关键词索引没有连上。
// 调用方应把它写成「BM25 未连接」，不要改用进程内分数冒充 Elasticsearch。
var ErrBM25Unavailable = errors.New("index: BM25 is not connected")

// NewDisconnectedBM25 是一个打开成功、检索失败的关键词注册表。
// 向量不在这里查。SearchKeyword 返回 ErrBM25Unavailable。
func NewDisconnectedBM25() Registry {
	return disconnectedBM25{}
}

type disconnectedBM25 struct{}

func (disconnectedBM25) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validCollection(id); err != nil {
		return nil, err
	}
	return disconnectedIndex{id: id}, nil
}

func (disconnectedBM25) Get(model.CollectionID) (Index, bool) {
	return nil, false
}

type disconnectedIndex struct {
	id model.CollectionID
}

func (d disconnectedIndex) Collection() model.CollectionID { return d.id }

func (d disconnectedIndex) BM25Connected() bool { return false }

func (d disconnectedIndex) Upsert(context.Context, []model.Chunk, []embed.Vector) error {
	return ErrBM25Unavailable
}

func (d disconnectedIndex) SearchVector(context.Context, Query) ([]Hit, error) {
	return nil, errors.New("index: vectors stay on the vector store")
}

func (d disconnectedIndex) SearchKeyword(ctx context.Context, q Query) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.CollectionID != d.id {
		return nil, errors.New("index: opened a different collection")
	}
	return nil, ErrBM25Unavailable
}
