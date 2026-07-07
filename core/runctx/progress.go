// progress.go:执行进度流(设计见 docs/channel-card-design.md §2)。
//
// 事件是执行域的事实(工具/模型调用的开始与结束),不是展示文案;
// 谁订阅、怎么呈现是订阅方的事。投递是结构性非阻塞的:有界队列 +
// 随 ctx 生命周期的投递 worker,发射侧永不等待订阅者——订阅者的
// 耗时/阻塞/panic 影响不到执行主流程(纪律靠 harness,不靠订阅者
// 自觉)。不装订阅即零开销。
package runctx

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// ScopeKind 枚举:该步骤是框架内部模块的,还是用户配置进来的能力的。
const (
	ScopeBuiltin = "builtin" // 内部模块:builtin 域能力(todo/ask_user/记忆等)、框架辅助生成(digest/压缩/评审重试)
	ScopeCustom  = "custom"  // 用户配置的能力(namespace 工具/技能/skillpack/子 agent)与业务模型轮次
)

// ProgressEvent 是一次执行步骤的进度事实(结构化,非展示文案)。
type ProgressEvent struct {
	// Seq 轮内序号,发射侧单调递增——被丢弃的事件留下可检测的序号
	// 缺口,订阅方据此感知有损。
	Seq int
	// Scope 执行域路径,与 runctx 执行域栈同源:"" = 主循环;段格式
	// comp:<名>#<序>(component/技能子循环)、sub:<名>(子 agent 委托),
	// 嵌套时段间以 \x1f 相连。域种类只在压栈处定义,事件不发明新词。
	Scope string
	// ScopeKind:builtin | custom(见常量)。能力事件按 Domain 判定,
	// 模型事件按框架内部标记判定。
	ScopeKind string
	// CapKind:能力事件透传 capability.Ref.Kind 原值(tool|skill|agent);
	// 模型轮次为 model。不新造枚举。
	CapKind string
	// Domain 是能力的注册域(builtin/catalog/sales...);模型事件为空。
	Domain string
	// Name 能力名 / 模型步骤名(框架辅助生成带自报 span 名,如 review/*)。
	Name string
	// Status:start | done | error。
	Status string
	// Dur 是 done/error 时的耗时(start 为 0)。
	Dur time.Duration
	// Detail 是参数摘要/结果摘要/错误摘要(已截断,防大 payload)。
	Detail string
}

// ProgressSink 是订阅者回调,由投递 worker 异步调用。
type ProgressSink func(ctx context.Context, ev ProgressEvent)

type progressCtxKey struct{}

// progressState 是一次订阅的投递状态:有界队列 + 序号发生器。
type progressState struct {
	ch      chan ProgressEvent
	seq     atomic.Int64
	dropped atomic.Int64
}

// progressBuffer 是投递队列容量:tool/model 粒度一轮通常 <30 事件,
// 64 富余;队列满丢新事件(进度是有损可接受的提示性信号,终稿收口
// 不走这条流,丢进度不丢结果)。
const progressBuffer = 64

// WithProgress 安装进度订阅:创建有界队列与投递 goroutine(生命周期
// 随 ctx,ctx 结束即退出)。发射侧对队列非阻塞写,队列满丢弃并计数。
func WithProgress(ctx context.Context, sink ProgressSink) context.Context {
	st := &progressState{ch: make(chan ProgressEvent, progressBuffer)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-st.ch:
				deliver(ctx, sink, ev)
			}
		}
	}()
	return context.WithValue(ctx, progressCtxKey{}, st)
}

// deliver 隔离订阅者 panic:订阅者出错不得中断投递,更不得中断执行。
func deliver(ctx context.Context, sink ProgressSink, ev ProgressEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("progress sink panicked", slog.Any("panic", r), slog.String("event", ev.Name))
		}
	}()
	sink(ctx, ev)
}

type keyBuiltinStep struct{}

// WithBuiltinStep 标记后续调用属于框架内部动作(digest 消化/压缩摘要/
// 评审重试等辅助生成)——发射点据此给模型事件定 ScopeKind=builtin。
func WithBuiltinStep(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyBuiltinStep{}, true)
}

// BuiltinStep 判定当前是否处于框架内部动作。
func BuiltinStep(ctx context.Context) bool {
	b, _ := ctx.Value(keyBuiltinStep{}).(bool)
	return b
}

// EmitProgress 发射一个进度事件:无订阅零开销;有订阅非阻塞入队,
// 队列满丢弃(Seq 缺口可检测)。供 runtime 发射点调用,业务侧无需关心。
func EmitProgress(ctx context.Context, ev ProgressEvent) {
	st, _ := ctx.Value(progressCtxKey{}).(*progressState)
	if st == nil {
		return
	}
	ev.Seq = int(st.seq.Add(1))
	select {
	case st.ch <- ev:
	default:
		st.dropped.Add(1)
	}
}
