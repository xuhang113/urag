package embed

import "context"

type stepHandlerKey struct{}

// WithStepHandler 接收本地前向进程写到额外描述符上的阶段记号。
// 没有安装处理函数时，记号被读掉后丢弃。
func WithStepHandler(ctx context.Context, fn func(token string)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, stepHandlerKey{}, fn)
}

// EmitStep 把一个阶段记号交给当前上下文里的处理函数。
func EmitStep(ctx context.Context, token string) {
	if ctx == nil || token == "" {
		return
	}
	fn, _ := ctx.Value(stepHandlerKey{}).(func(string))
	if fn != nil {
		fn(token)
	}
}

func hasStepHandler(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	fn, _ := ctx.Value(stepHandlerKey{}).(func(string))
	return fn != nil
}
