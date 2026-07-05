package runctx

import "context"

// fork 标志:声明本次调用请求继承调用方上下文(由使用点包装设置,
// 如 graph 步骤的 context: fork)。标志本身是纯 ctx 布尔,放基座 runctx,
// 使 engine(编排)与 loop(消息组装 ForkMessages)都能读到,不成环。
// 快照/消息组装(需 eino schema)仍在 loop/fork.go。
type keyFork struct{}

// WithForkContext 声明本次调用请求继承调用方上下文。
func WithForkContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyFork{}, true)
}

// ForkRequested 报告本次调用是否请求了上下文继承。
func ForkRequested(ctx context.Context) bool {
	b, _ := ctx.Value(keyFork{}).(bool)
	return b
}
