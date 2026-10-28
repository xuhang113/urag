package job

import (
	"context"
	"errors"
	"strings"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// publicCollection 是试查页唯一打开的集合。
const publicCollection model.CollectionID = "public"

// Try 只在集合 public 里检索。向量走注册表里的向量索引，BM25 走关键词索引。
// 合并后留下前 50 条子块，再留下最多 8 条。
// 第二个返回值表示这些分数来自重排器。只有这时才丢掉没有高于 RerankMinScore 的命中，可以返回 0 条。
// 没有重排分时按混合分返回这最多 8 条，不套门槛。
// KeepEmbed 时嵌入进程留到试查进程结束，提问结束不关掉它。
// 否则嵌入器若能 Close，会在查询向量返回前关掉，然后再做向量检索和重排。
// 嵌入进程还在时不启动重排模型。重排后若有重排分，先按 RerankMinScore 丢掉低分，再取父块。
// ctx 取消或到点时，这些步骤一起停。
// 其它集合不会被打开。query 空白时返回错误。
// 夹具评价不走这里。
func (p *Pipeline) Try(ctx context.Context, query string) ([]retrieve.Hit, bool, error) {
	if p == nil || p.deps.Embedder == nil || p.deps.Registry == nil {
		return nil, false, errMissingDeps
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("job: query required")
	}
	if p.deps.Passages == nil {
		return nil, false, errors.New("job: nil reranker")
	}
	p.tryMu.Lock()
	defer p.tryMu.Unlock()

	emb := p.deps.Embedder
	if p.cfg.KeepEmbed {
		if session, ok := emb.(embed.ForwardSession); ok && (session.Holding() || session.ProcessID() > 0) {
			pid := session.ProcessID()
			p.notedEmbedPID = pid
			ctx = retrieve.WithRerankPlan(ctx, retrieve.RerankPlan{
				SkipModel: true,
				EmbedPID:  p.notedEmbedPID,
				Park:      p.parkEmbedForRerank,
			})
		}
	} else if closer, ok := emb.(interface{ Close() error }); ok {
		emb = retrieve.ReleaseOnEmbed(emb, closer.Close)
	}
	svc := retrieve.New(p.deps.Registry, emb, p.cfg.Weights, nil)
	ctx = retrieve.WithRerankScores(ctx)
	hits, err := svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{publicCollection},
		Query:       query,
	}, p.deps.Passages, nil)
	if err != nil {
		return nil, false, err
	}
	reranked := retrieve.HasRerankScores(ctx)
	if reranked {
		filter := retrieve.StepFilter(p.cfg.RerankMinScore)
		retrieve.Report(ctx, filter, false)
		hits = retrieve.KeepAboveRerankScore(hits, p.cfg.RerankMinScore)
		retrieve.Report(ctx, filter, true)
	}
	retrieve.Report(ctx, retrieve.StepParents, false)
	hits, err = retrieve.FillParents(ctx, hits, p.deps.Parents)
	if err != nil {
		return nil, reranked, err
	}
	retrieve.Report(ctx, retrieve.StepParents, true)
	return hits, reranked, nil
}

// parkEmbedForRerank 只在第一次重排前写下取舍。
// 嵌入进程不退出。之后的提问不再写这一行，也不再冷启动嵌入模型。
func (p *Pipeline) parkEmbedForRerank(ctx context.Context) {
	if p == nil || !p.cfg.ParkEmbed || p.parked {
		return
	}
	p.parked = true
	retrieve.Report(ctx, retrieve.StepParkEmbed, false)
	retrieve.Report(ctx, retrieve.StepParkEmbed, true)
}
