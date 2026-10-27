package job

import (
	"context"
	"errors"
	"strings"

	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// publicCollection 是试查页唯一打开的集合。
const publicCollection model.CollectionID = "public"

// Try 只在集合 public 里做磁盘向量检索和该集合的 BM25。
// 合并后留下前 50 条子块，重排后最多 8 条，再丢掉没有高于 RerankMinScore 的命中。
// 可以返回 0 条。嵌入器若能 Close，会在查询向量返回前关掉，然后再调用重排。
// 其它集合不会被打开。query 空白时返回错误。
// 夹具评价不走这里。
func (p *Pipeline) Try(ctx context.Context, query string) ([]retrieve.Hit, error) {
	if p == nil || p.deps.Embedder == nil || p.deps.Registry == nil {
		return nil, errMissingDeps
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("job: query required")
	}
	if p.deps.Passages == nil {
		return nil, errors.New("job: nil reranker")
	}
	p.tryMu.Lock()
	defer p.tryMu.Unlock()

	emb := p.deps.Embedder
	if closer, ok := emb.(interface{ Close() error }); ok {
		emb = retrieve.ReleaseOnEmbed(emb, closer.Close)
	}
	svc := retrieve.New(p.deps.Registry, emb, p.cfg.Weights, nil)
	hits, err := svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{publicCollection},
		Query:       query,
	}, p.deps.Passages, p.deps.Parents)
	if err != nil {
		return nil, err
	}
	return retrieve.KeepAboveRerankScore(hits, p.cfg.RerankMinScore), nil
}
