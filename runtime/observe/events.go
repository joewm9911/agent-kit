// events.go:结构化进度事件的发射点(设计见 docs/channel-card-design.md §2.3)。
//
// 与 Progress(终端文本行)同源:同一个 eino callbacks 切面看见每次
// 模型/工具调用,这里把它们转成 runctx.ProgressEvent 发给 ctx 里的
// 订阅(runctx.WithProgress 安装;未安装零开销)。engine 零改动。
package observe

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
)

var progressEventsOnce sync.Once

// EnsureProgressEvents 把进度事件发射切面挂到全局 callbacks(幂等,
// 多次调用只挂一次)。dispatcher 构造时调用;直连 agent.Run 的宿主
// 需要事件时自行调用。
func EnsureProgressEvents() {
	progressEventsOnce.Do(func() {
		callbacks.AppendGlobalHandlers(ProgressEvents())
	})
}

type eventStartKey struct{}

// ProgressEvents 返回进度事件发射 Handler。只发射 ChatModel(模型
// 轮次与框架辅助生成);能力事件由 loop.ProgressTools 在能力层发射。
func ProgressEvents() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			emitStart(ctx, info, input)
			return context.WithValue(ctx, eventStartKey{}, time.Now())
		}).
		OnStartWithStreamInputFn(func(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
			input.Close() // 私有副本,事件不需要入参内容
			emitStart(ctx, info, nil)
			return context.WithValue(ctx, eventStartKey{}, time.Now())
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			emitEnd(ctx, info, output, nil)
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			go func() {
				defer output.Close()
				emitEnd(ctx, info, concatStream(info, output), nil)
			}()
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			emitEnd(ctx, info, nil, err)
			return ctx
		}).
		Build()
}

// eventKind:能力事件由能力层发射(loop.ProgressTools,拿得到
// Ref.Kind/Domain 真值),本切面只发模型事件;其余组件忽略。
func eventKind(info *callbacks.RunInfo) string {
	if info == nil || string(info.Component) != "ChatModel" {
		return ""
	}
	return "model"
}

// eventName:业务轮次统一叫「模型」;框架辅助生成保留自报 span 名
// (review/finish、digest 等)。
func eventName(info *callbacks.RunInfo, _ string) string {
	if info.Name != "" {
		return info.Name
	}
	return "模型"
}

// modelEvent 构造模型事件骨架:Scope 取执行域栈,ScopeKind 按内部
// 动作标记判定(digest/压缩/评审重试 = builtin,业务轮次 = custom)。
func modelEvent(ctx context.Context, info *callbacks.RunInfo) runctx.ProgressEvent {
	kind := runctx.ScopeCustom
	if runctx.BuiltinStep(ctx) {
		kind = runctx.ScopeBuiltin
	}
	return runctx.ProgressEvent{
		Scope:     runctx.Scope(ctx),
		ScopeKind: kind,
		CapKind:   "model",
		Name:      eventName(info, ""),
	}
}

func emitStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) {
	kind := eventKind(info)
	if kind == "" {
		return
	}
	_ = input
	ev := modelEvent(ctx, info)
	ev.Status = "start"
	runctx.EmitProgress(ctx, ev)
}

func emitEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput, err error) {
	kind := eventKind(info)
	if kind == "" {
		return
	}
	ev := modelEvent(ctx, info)
	if t, ok := ctx.Value(eventStartKey{}).(time.Time); ok {
		ev.Dur = time.Since(t)
	}
	if err != nil {
		ev.Status = "error"
		ev.Detail = truncate(err.Error(), 120)
		runctx.EmitProgress(ctx, ev)
		return
	}
	ev.Status = "done"
	// 模型步骤的"决定":调了哪些工具,或输出了什么。
	if mo := einomodel.ConvCallbackOutput(output); mo != nil && mo.Message != nil {
		if len(mo.Message.ToolCalls) > 0 {
			names := make([]string, 0, len(mo.Message.ToolCalls))
			for _, tc := range mo.Message.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			ev.Detail = "决定调用 " + strings.Join(names, ", ")
		} else {
			ev.Detail = truncate(mo.Message.Content, 120)
		}
	}
	runctx.EmitProgress(ctx, ev)
}
