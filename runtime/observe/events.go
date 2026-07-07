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
	einotool "github.com/cloudwego/eino/components/tool"
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

// ProgressEvents 返回进度事件发射 Handler。只发射 Tool 与 ChatModel
// 两类组件(与 Progress 文本行同粒度;技能以工具形态出现,Kind=tool)。
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

// eventKind 把 eino 组件映射为事件 Kind;不关心的组件返回空。
func eventKind(info *callbacks.RunInfo) string {
	if info == nil {
		return ""
	}
	switch string(info.Component) {
	case "Tool":
		if info.Name == "" {
			return ""
		}
		return "tool"
	case "ChatModel":
		return "model"
	}
	return ""
}

func eventName(info *callbacks.RunInfo, kind string) string {
	if kind == "model" {
		return "模型"
	}
	return info.Name
}

func emitStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) {
	kind := eventKind(info)
	if kind == "" {
		return
	}
	detail := ""
	if kind == "tool" {
		if in := einotool.ConvCallbackInput(input); in != nil {
			detail = truncate(in.ArgumentsInJSON, 120)
		}
	}
	runctx.EmitProgress(ctx, runctx.ProgressEvent{
		Kind: kind, Name: eventName(info, kind), Status: "start", Detail: detail,
	})
}

func emitEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput, err error) {
	kind := eventKind(info)
	if kind == "" {
		return
	}
	var dur time.Duration
	if t, ok := ctx.Value(eventStartKey{}).(time.Time); ok {
		dur = time.Since(t)
	}
	ev := runctx.ProgressEvent{Kind: kind, Name: eventName(info, kind), Dur: dur}
	if err != nil {
		ev.Status = "error"
		ev.Detail = truncate(err.Error(), 120)
		runctx.EmitProgress(ctx, ev)
		return
	}
	ev.Status = "done"
	switch kind {
	case "tool":
		if s, ok := output.(string); ok {
			ev.Detail = truncate(s, 120)
		}
	case "model":
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
	}
	runctx.EmitProgress(ctx, ev)
}
