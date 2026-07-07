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

// ProgressEvent 是一次执行步骤的进度事实(结构化,非展示文案)。
type ProgressEvent struct {
	// Seq 轮内序号,发射侧单调递增——被丢弃的事件留下可检测的序号
	// 缺口,订阅方据此感知有损。
	Seq int
	// Kind:tool | model(技能以工具形态挂在工具面上,呈现为 tool)。
	Kind string
	// Name 能力名/模型步骤名。
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
