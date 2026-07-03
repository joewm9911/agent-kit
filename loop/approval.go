package loop

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/cloverzhang/agent-kit/capability"
	"github.com/cloverzhang/agent-kit/runctx"
)

// ApprovalMode 决定改动性操作的放行策略。
type ApprovalMode string

const (
	// ApprovalAuto 自动放行(可信环境/离线批处理)。
	ApprovalAuto ApprovalMode = "auto"
	// ApprovalInteractive 经 runctx 的交互通道请求批准(CLI/飞书卡片)。
	ApprovalInteractive ApprovalMode = "interactive"
	// ApprovalDeny 一律拒绝(只读部署)。
	ApprovalDeny ApprovalMode = "deny"
)

// GateApproval 给能力集套上审批闸门:Risk ≥ mutating 的能力在执行前
// 按 mode 处理。拒绝结果以工具返回值回传模型(而非错误),让大脑
// 换路径或向用户说明,循环不中断。
func GateApproval(caps []capability.Capability, mode ApprovalMode) []capability.Capability {
	if mode == "" || mode == ApprovalAuto {
		return caps
	}
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if c.Meta().Risk >= capability.RiskMutating {
			out = append(out, &gated{inner: c, mode: mode})
		} else {
			out = append(out, c)
		}
	}
	return out
}

type gated struct {
	inner capability.Capability
	mode  ApprovalMode
}

func (g *gated) Meta() capability.Meta { return g.inner.Meta() }

func (g *gated) AsTool(ctx context.Context) (tool.BaseTool, error) {
	t, err := g.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := t.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", g.inner.Meta().Ref)
	}
	return &gatedTool{inner: inv, g: g}, nil
}

func (g *gated) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return g.run(ctx, argsJSON, func(ctx context.Context) (string, error) {
			return capability.Invoke(ctx, g.inner, argsJSON)
		})
	}), nil
}

func (g *gated) run(ctx context.Context, argsJSON string, exec func(ctx context.Context) (string, error)) (string, error) {
	meta := g.inner.Meta()
	if g.mode == ApprovalDeny {
		return fmt.Sprintf("操作被拒绝:%s 是改动性操作,当前部署为只读模式。", meta.Ref.Name), nil
	}
	it := runctx.GetInteractor(ctx)
	if it == nil {
		return fmt.Sprintf("操作未执行:%s 需要人工批准,但当前无交互通道。请向用户说明情况。", meta.Ref.Name), nil
	}
	ok, err := it.Approve(ctx, runctx.ApprovalRequest{
		CapRef:      meta.Ref.String(),
		Description: meta.Description,
		Arguments:   argsJSON,
	})
	if err != nil {
		return fmt.Sprintf("操作未执行:批准流程失败(%v)。", err), nil
	}
	if !ok {
		return fmt.Sprintf("操作未执行:用户拒绝了 %s 的本次调用。请调整方案或询问用户意图。", meta.Ref.Name), nil
	}
	return exec(ctx)
}

type gatedTool struct {
	inner tool.InvokableTool
	g     *gated
}

func (t *gatedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx) // 短名与描述保持不变,审批对模型透明
}

func (t *gatedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return t.g.run(ctx, argsJSON, func(ctx context.Context) (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}
