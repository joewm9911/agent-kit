package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

// DefaultToolTimeout 是工具单次调用的默认超时。MCP 等外部工具可能
// 无限悬挂,不设防会挂死整个循环。
const DefaultToolTimeout = 5 * time.Minute

// TimeoutTools 给能力集套上单次调用超时(Ring 0)。超时以工具结果
// 回传模型(而非错误),让大脑换路径推进,循环不中断;宿主 ctx 被
// 取消时则原样传播错误。d==0 用默认值,<0 关闭。
//
// 两类豁免:审批闸门应在本闸外侧(人工批准的等待不计入执行超时);
// 带 TagInteractive 的交互类能力(ask_user 等)不套闸——等人回复的
// 时间不是执行时间。
func TimeoutTools(caps []capability.Capability, d time.Duration) []capability.Capability {
	if d < 0 {
		return caps
	}
	if d == 0 {
		d = DefaultToolTimeout
	}
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if hasTag(c.Meta().Tags, capability.TagInteractive) {
			out = append(out, c)
			continue
		}
		out = append(out, &timeoutCap{inner: c, d: d})
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

type timeoutCap struct {
	inner capability.Capability
	d     time.Duration
}

func (t *timeoutCap) Meta() capability.Meta { return t.inner.Meta() }

func (t *timeoutCap) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := t.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", t.inner.Meta().Ref)
	}
	return &timeoutTool{inner: inv, name: t.inner.Meta().Ref.Name, d: t.d}, nil
}

func (t *timeoutCap) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return runWithTimeout(ctx, t.inner.Meta().Ref.Name, t.d, func(ctx context.Context) (string, error) {
			return capability.Invoke(ctx, t.inner, argsJSON)
		})
	}), nil
}

type timeoutTool struct {
	inner tool.InvokableTool
	name  string
	d     time.Duration
}

func (t *timeoutTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx) // 超时对模型透明
}

func (t *timeoutTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return runWithTimeout(ctx, t.name, t.d, func(ctx context.Context) (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}

type toolResult struct {
	out string
	err error
}

// runWithTimeout 在独立 goroutine 里执行工具并限时等待。工具实现
// 若无视 ctx 取消,该 goroutine 会泄漏到工具自然返回为止——这是
// 对"不可控外部工具"能做到的最好兜底。
func runWithTimeout(ctx context.Context, name string, d time.Duration, exec func(ctx context.Context) (string, error)) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	done := make(chan toolResult, 1)
	go func() {
		out, err := exec(tctx)
		done <- toolResult{out, err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-tctx.Done():
		if ctx.Err() != nil {
			return "", ctx.Err() // 宿主取消,原样传播
		}
		return fmt.Sprintf("操作未完成:%s 执行超过 %s 已中止。可缩小参数范围后重试,或改用其他方式。", name, d), nil
	}
}
