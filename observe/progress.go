package observe

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// Progress 返回面向终端用户的进度提示 Handler,让用户看到 agent
// 正在做什么,而不只是"在跑":
//
//	⚙ get_weather({"city":"北京"})
//	✓ get_weather (1.4s) → 北京:晴,26 度
//	· 模型 (2.1s) → 决定调用 write_file
//	· 模型 (8.3s) → “报告已完成,主要结论是...”
//
// 同时覆盖 invoke 与 stream 两种执行范式(流式回调收到的是私有
// 流副本,消费后关闭)。callbacks 是全局切面,skill / plan-execute
// 内部的每一步同样可见。机器可读的完整轨迹见 Trajectory。
func Progress(w io.Writer) callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			printToolStart(w, info, input)
			return context.WithValue(ctx, progressKey{}, time.Now())
		}).
		OnStartWithStreamInputFn(func(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
			// 流式入参:取第一帧展示,随即关闭副本。
			first, err := input.Recv()
			input.Close()
			if err == nil {
				printToolStart(w, info, first)
			} else {
				printToolStart(w, info, nil)
			}
			return context.WithValue(ctx, progressKey{}, time.Now())
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			printEnd(ctx, w, info, output)
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			// 流式出参:后台消费私有副本,聚合后展示(不阻塞主流)。
			go func() {
				defer output.Close()
				printEnd(ctx, w, info, concatStream(info, output))
			}()
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			if info != nil && info.Name != "" {
				fmt.Fprintf(w, "  ✗ %s 失败: %v\n", info.Name, err)
			}
			return ctx
		}).
		Build()
}

type progressKey struct{}

func printToolStart(w io.Writer, info *callbacks.RunInfo, input callbacks.CallbackInput) {
	if info == nil || string(info.Component) != "Tool" || info.Name == "" {
		return
	}
	args := ""
	if in := einotool.ConvCallbackInput(input); in != nil {
		args = truncate(in.ArgumentsInJSON, 80)
	}
	fmt.Fprintf(w, "  ⚙ %s(%s)\n", info.Name, args)
}

// concatStream 把流式输出聚合为单个 CallbackOutput 以复用展示逻辑。
func concatStream(info *callbacks.RunInfo, sr *schema.StreamReader[callbacks.CallbackOutput]) callbacks.CallbackOutput {
	isModel := info != nil && string(info.Component) == "ChatModel"
	var msgs []*schema.Message
	var sb strings.Builder
	for {
		chunk, err := sr.Recv()
		if err != nil {
			break
		}
		if isModel {
			if mo := einomodel.ConvCallbackOutput(chunk); mo != nil && mo.Message != nil {
				msgs = append(msgs, mo.Message)
			}
		} else if s, ok := chunk.(string); ok {
			sb.WriteString(s)
		}
	}
	if isModel {
		if len(msgs) == 0 {
			return nil
		}
		full, err := schema.ConcatMessages(msgs)
		if err != nil {
			return nil
		}
		return &einomodel.CallbackOutput{Message: full}
	}
	return sb.String()
}

func printEnd(ctx context.Context, w io.Writer, info *callbacks.RunInfo, output callbacks.CallbackOutput) {
	if info == nil || info.Name == "" {
		return
	}
	elapsed := ""
	if t, ok := ctx.Value(progressKey{}).(time.Time); ok {
		elapsed = fmt.Sprintf(" (%.1fs)", time.Since(t).Seconds())
	}

	switch string(info.Component) {
	case "Tool":
		result := ""
		if s, ok := output.(string); ok && s != "" {
			result = " → " + truncate(s, 80)
		}
		fmt.Fprintf(w, "  ✓ %s%s%s\n", info.Name, elapsed, result)

	case "ChatModel":
		// 展示模型这一步的"决定":调用了哪个工具,或输出了什么内容。
		detail := ""
		if mo := einomodel.ConvCallbackOutput(output); mo != nil && mo.Message != nil {
			if len(mo.Message.ToolCalls) > 0 {
				names := make([]string, 0, len(mo.Message.ToolCalls))
				for _, tc := range mo.Message.ToolCalls {
					names = append(names, tc.Function.Name)
				}
				detail = " → 决定调用 " + strings.Join(names, ", ")
			} else if mo.Message.Content != "" {
				detail = " → “" + truncate(mo.Message.Content, 60) + "”"
			}
		}
		fmt.Fprintf(w, "  · 模型%s%s\n", elapsed, detail)
	}
}

// truncate 截断到 n 个字符(rune 安全),换行折叠为空格。
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
