package retrieve

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
)

const (
	// StepLoadEmbed 从开始等待嵌入模型到权重进入内存。
	StepLoadEmbed = "加载嵌入模型"
	// StepEmbedResident 表示权重已经在这次试查进程里，这次提问不再加载。
	StepEmbedResident = "嵌入模型已在内存"
	// StepQueryVector 是查询向量的前向，发生在模型已经加载之后。
	StepQueryVector = "计算查询向量"
	// StepVectorSearch 是磁盘或内存上的向量检索。
	StepVectorSearch = "向量检索"
	// StepBM25 是当前集合的 BM25。Elasticsearch 连着时才用这个名字。
	StepBM25 = "BM25"
	// StepBM25Offline 表示这次没有连上 Elasticsearch，不能把进程内分数写成 BM25。
	StepBM25Offline = "BM25 未连接"
	// StepMergeTop50 是向量和 BM25 合并后留下的前 50 条。
	StepMergeTop50 = "合并前 50 条"
	// StepUnloadEmbed 是嵌入进程退出。它发生在向量检索之前，因为查询向量一返回进程就退出。
	StepUnloadEmbed = "退出嵌入模型"
	// StepParkEmbed 是内存放不下嵌入和重排两个模型时，第一次重排前写下的取舍。
	// 嵌入进程继续留着，这次不再启动重排模型。
	StepParkEmbed = "为重排暂时卸下嵌入"
	// StepLoadRerank 是重排权重进入内存。嵌入进程退出之后才会发生。
	StepLoadRerank = "加载重排模型"
	// StepRerank 是对这 50 条打分。
	StepRerank = "重排"
	// StepUnloadRerank 是重排进程退出，权重离开内存。
	StepUnloadRerank = "退出重排模型"
	// StepParents 是按父块编号取整节正文。
	StepParents = "取父块"
)

// StepFilter 是按重排门槛丢掉低分命中的阶段名。
// 门槛是这次查询真正使用的分数，0.5 会写成「按 0.5 分过滤」。
func StepFilter(min float64) string {
	return "按 " + strconv.FormatFloat(min, 'f', -1, 64) + " 分过滤"
}

// ProgressFunc 接收一个阶段。done 为 false 表示这一步已经开始、还没做完。
// 同名阶段完成后再次报告会被忽略，避免把已经结束的步骤又标成未完成。
type ProgressFunc func(name string, done bool)

type progressKey struct{}

type progress struct {
	mu      sync.Mutex
	fn      ProgressFunc
	started map[string]bool
	done    map[string]bool
}

// WithProgress 让这次查询把阶段报告给 fn。
// fn 可以为空，这时仍记录完成状态，供同一次调用判断某步是否已经结束。
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, progressKey{}, &progress{
		fn:      fn,
		started: map[string]bool{},
		done:    map[string]bool{},
	})
}

func progressFrom(ctx context.Context) *progress {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(progressKey{}).(*progress)
	return p
}

// Report 记录阶段。没有进度监听时什么也不做。
// 已经完成的阶段不再改回未完成。重复的开始报告只保留第一次。
func Report(ctx context.Context, name string, done bool) {
	p := progressFrom(ctx)
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	if p.done[name] {
		p.mu.Unlock()
		return
	}
	if !done && p.started[name] {
		p.mu.Unlock()
		return
	}
	p.started[name] = true
	if done {
		p.done[name] = true
	}
	fn := p.fn
	p.mu.Unlock()
	if fn != nil {
		fn(name, done)
	}
}

// StepDone 报告这个阶段是否已经完成。没有进度监听时返回 false。
func StepDone(ctx context.Context, name string) bool {
	p := progressFrom(ctx)
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done[name]
}

// bindModelSteps 把嵌入进程额外描述符上的阶段记号接到这次查询。
// 记号只在进程真正写出来之后才标完成，调用方不能提前把后面的步骤标成完成。
func bindModelSteps(ctx context.Context) context.Context {
	parent := ctx
	return embed.WithStepHandler(parent, func(token string) {
		noteModelStep(parent, token)
	})
}

// RerankPlan 告诉这次查询怎么处理重排。
// SkipModel 为 true 时不启动重排模型。EmbedPID 是重排前记下的嵌入进程，没有则为 0。
// Park 在第一次需要说明取舍时写页面日志，之后由调用方自己跳过。
type RerankPlan struct {
	SkipModel bool
	EmbedPID  int
	Park      func(context.Context)
}

type rerankPlanKey struct{}

// WithRerankPlan 把这次查询的重排取舍放进上下文。
func WithRerankPlan(ctx context.Context, plan RerankPlan) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rerankPlanKey{}, plan)
}

func rerankPlanFrom(ctx context.Context) (RerankPlan, bool) {
	if ctx == nil {
		return RerankPlan{}, false
	}
	plan, ok := ctx.Value(rerankPlanKey{}).(RerankPlan)
	return plan, ok
}

// rerankScoreBox 记录这次查询的重排器是否返回了分数。
// 只复制混合分时不记。试查用它决定要不要套分数门槛。
type rerankScoreBox struct {
	mu  sync.Mutex
	got bool
}

type rerankScoreKey struct{}

// WithRerankScores 让这次查询可以记下重排器是否返回了分数。
// 已经放过时返回原上下文，方便调用方和检索共用同一个标记。
func WithRerankScores(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(rerankScoreKey{}).(*rerankScoreBox); ok {
		return ctx
	}
	return context.WithValue(ctx, rerankScoreKey{}, &rerankScoreBox{})
}

func markRerankScores(ctx context.Context) {
	if ctx == nil {
		return
	}
	box, _ := ctx.Value(rerankScoreKey{}).(*rerankScoreBox)
	if box == nil {
		return
	}
	box.mu.Lock()
	box.got = true
	box.mu.Unlock()
}

// HasRerankScores 报告这次查询的重排器是否返回了分数。
// 没有用 WithRerankScores 准备过，或只沿用了混合分时，返回 false。
func HasRerankScores(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	box, _ := ctx.Value(rerankScoreKey{}).(*rerankScoreBox)
	if box == nil {
		return false
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.got
}

func noteModelStep(ctx context.Context, token string) {
	if noteDevice(ctx, token, "embed_device_done ", true) || noteDevice(ctx, token, "embed_device ", false) {
		return
	}
	if noteRerankModel(ctx, token) {
		return
	}
	switch token {
	case "load_embed_done":
		Report(ctx, StepLoadEmbed, true)
		Report(ctx, StepQueryVector, false)
	case "query_vector_done":
		Report(ctx, StepQueryVector, true)
	case "load_rerank_done":
		Report(ctx, StepLoadRerank, true)
		Report(ctx, StepRerank, false)
	case "rerank_done":
		Report(ctx, StepRerank, true)
	case "unload_embed":
		// 短生命周期进程没有单独的记号时，退出发生在 Embed 返回前。
		// 这时查询向量已经算完，不能把退出标到向量完成之前。
		if !StepDone(ctx, StepQueryVector) {
			Report(ctx, StepLoadEmbed, true)
			Report(ctx, StepQueryVector, true)
		}
		Report(ctx, StepUnloadEmbed, false)
	case "unload_embed_done":
		Report(ctx, StepUnloadEmbed, true)
	case "unload_rerank":
		Report(ctx, StepUnloadRerank, false)
	case "unload_rerank_done":
		Report(ctx, StepUnloadRerank, true)
	}
}

// noteRerankModel 把重排进程报上来的模型名写成页面步骤。
// 名称形如「重排模型 BAAI/bge-reranker-base」。done 前缀必须先匹配。
func noteRerankModel(ctx context.Context, token string) bool {
	const donePrefix = "rerank_model_done "
	const startPrefix = "rerank_model "
	prefix := startPrefix
	done := false
	if strings.HasPrefix(token, donePrefix) {
		prefix = donePrefix
		done = true
	} else if !strings.HasPrefix(token, startPrefix) {
		return false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(token, prefix))
	if spec == "" || strings.ContainsAny(spec, "\n\r") {
		return false
	}
	Report(ctx, RerankModelStep(spec), done)
	return true
}

// noteDevice 把嵌入进程报上来的 device 和 dtype 写成页面步骤。
// 名称形如「嵌入设备 mps float16」。done 前缀必须先匹配，避免被开始前缀截走。
func noteDevice(ctx context.Context, token, prefix string, done bool) bool {
	if !strings.HasPrefix(token, prefix) {
		return false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(token, prefix))
	if spec == "" || strings.ContainsAny(spec, "\n\r") {
		return false
	}
	Report(ctx, "嵌入设备 "+spec, done)
	return true
}
