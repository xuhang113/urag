package embed

import (
	"context"
	"fmt"
	"sync"
)

// 进程里同时只驻留一种本地模型。嵌入是 embed，重排是 rerank。
const (
	KindEmbed  = "embed"
	KindRerank = "rerank"
)

// MaxForwardBatch 是一次本地前向允许留在 Go 进程里的文本条数。
// 调用方要按批切开。超过这个数就拒绝，避免把整库向量放进内存。
const MaxForwardBatch = 256

var localModels struct {
	mu     sync.Mutex
	cond   *sync.Cond
	held   string
	notify func(waiting, held string)
}

func init() {
	localModels.cond = sync.NewCond(&localModels.mu)
}

// SetLocalWaitNotifier 在一种模型还占着闸、另一种开始等待时触发。
// 通知在闸的锁里执行，不能再启动前向，否则会卡住。
// 测试用来确认嵌入进程退出后才放行重排。
func SetLocalWaitNotifier(fn func(waiting, held string)) {
	localModels.mu.Lock()
	localModels.notify = fn
	localModels.mu.Unlock()
}

// WithLocalModel 在 fn 运行期间占住进程内的模型闸，fn 返回后释放。
// 另一种模型还在驻留时，一直等到它退出再进入。同一时刻只有一种模型。
func WithLocalModel(ctx context.Context, kind string, fn func() error) error {
	if kind != KindEmbed && kind != KindRerank {
		return fmt.Errorf("embed: unknown local model %q", kind)
	}
	if fn == nil {
		return fmt.Errorf("embed: nil loader for %s", kind)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acquireLocal(ctx, kind); err != nil {
		return err
	}
	defer releaseLocal()
	return fn()
}

// GuardLocalModel 在 load 运行期间占住模型闸，load 返回后释放。
// 这样嵌入进程结束后，重排才可以加载。失败时也不会把 kind 留下。
func GuardLocalModel(kind string, load func() error) error {
	return WithLocalModel(context.Background(), kind, load)
}

func acquireLocal(ctx context.Context, kind string) error {
	stop := context.AfterFunc(ctx, func() {
		localModels.mu.Lock()
		localModels.cond.Broadcast()
		localModels.mu.Unlock()
	})
	defer stop()

	localModels.mu.Lock()
	defer localModels.mu.Unlock()
	for localModels.held != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if localModels.notify != nil {
			localModels.notify(kind, localModels.held)
		}
		localModels.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	localModels.held = kind
	return nil
}

func releaseLocal() {
	localModels.mu.Lock()
	localModels.held = ""
	localModels.cond.Broadcast()
	localModels.mu.Unlock()
}
