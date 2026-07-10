package serving

import (
	"context"
	"sync"
)

// InboundInfo 是交给 ContextHook 的传输中立请求身份(HTTP/A2A/IM 统一口径)。
type InboundInfo struct {
	Channel string // "http" | "a2a" | 通道名(如 feishu)
	User    string // 终端用户身份(可空)
	Session string // 会话标识
}

// ContextHook 在**每个入站轮次**执行 agent 前、于入口边界丰富其 context——
// HTTP /messages、A2A、IM 三个入口统一经过。第三方注册它,把传输层 baggage
// (trace logid、租户、correlation id 等)提升/派生进 context,从而一路流到
// agent、工具与 decorator。
//
// 调用时机:ctx 已含**存活的传输 baggage**(入口用 WithoutCancel 保值)与
// runctx 用户身份;钩子在此之上叠加。返回丰富后的 ctx;不需要改动则原样返回。
type ContextHook func(ctx context.Context, info InboundInfo) context.Context

var (
	hookMu sync.RWMutex
	hooks  []ContextHook
)

// RegisterContextHook 注册一个入口层 context 丰富器(init/装配期调用)。多个
// 钩子按注册顺序叠加;nil 忽略。注册后只读,契合"注册表装配期只读"约束。
func RegisterContextHook(h ContextHook) {
	if h == nil {
		return
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	hooks = append(hooks, h)
}

// applyContextHooks 按注册顺序运行全部钩子;无注册则原样返回(零成本旁路)。
// 每轮入站执行一次,非每 token,锁开销可忽略。快照后在锁外执行——钩子内
// 再调 RegisterContextHook 不会自死锁(RWMutex 不可重入)。
func applyContextHooks(ctx context.Context, info InboundInfo) context.Context {
	hookMu.RLock()
	snapshot := hooks
	hookMu.RUnlock()
	for _, h := range snapshot {
		ctx = h(ctx, info)
	}
	return ctx
}
