package loop

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
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

// GateApprovalCtx 同 GateApproval,但放行策略在调用时从 ctx 解析
// (由 agent 经 WithApprovalMode 装入,缺省 interactive)。skill 与
// component 的内部工具面用它套闸:同一能力被不同审批策略的 agent
// 复用时,各自的模式经 ctx 生效,内部改动操作不再因"skill 边界批准
// 一次"而裸奔。
func GateApprovalCtx(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if c.Meta().Risk >= capability.RiskMutating {
			out = append(out, &gated{inner: c}) // mode 留空 = 运行时从 ctx 解析
		} else {
			out = append(out, c)
		}
	}
	return out
}

type keyApprovalMode struct{}

// WithApprovalMode 把审批模式装入 ctx,对下游 GateApprovalCtx 套闸的
// 能力生效。
func WithApprovalMode(ctx context.Context, mode ApprovalMode) context.Context {
	if mode == "" {
		return ctx
	}
	return context.WithValue(ctx, keyApprovalMode{}, mode)
}

func approvalModeFrom(ctx context.Context) ApprovalMode {
	m, _ := ctx.Value(keyApprovalMode{}).(ApprovalMode)
	return m
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
	st := approvalStateFrom(ctx)

	mode := g.mode
	if mode == "" { // ctx 套闸:运行时解析,缺省 interactive
		if st != nil && st.Mode != "" {
			mode = st.Mode
		} else if mode = approvalModeFrom(ctx); mode == "" {
			mode = ApprovalInteractive
		}
	}
	if mode == ApprovalAuto {
		return exec(ctx)
	}
	if mode == ApprovalDeny {
		return fmt.Sprintf("操作被拒绝:%s 是改动性操作,当前部署为只读模式。", meta.Ref.Name), nil
	}

	// 参数级策略:首条命中生效(allow 免批 / deny 直拒 / ask 照常)。
	if st != nil {
		switch st.decide(meta.Ref, argsJSON) {
		case actionAllow:
			return exec(ctx)
		case actionDeny:
			return fmt.Sprintf("操作被拒绝:%s 的本次调用命中审批策略的 deny 规则。请调整参数或换路径。", meta.Ref.Name), nil
		}
		// 决策记忆:用户此前对该能力选择过"总是允许/拒绝"
		if allowed, ok := st.recall(ctx, meta.Ref.Key()); ok {
			if allowed {
				return exec(ctx)
			}
			return fmt.Sprintf("操作未执行:用户已在本会话拒绝 %s 的后续调用。", meta.Ref.Name), nil
		}
	}

	it := runctx.GetInteractor(ctx)
	if it == nil {
		return fmt.Sprintf("操作未执行:%s 需要人工批准,但当前无交互通道。请向用户说明情况。", meta.Ref.Name), nil
	}
	req := runctx.ApprovalRequest{
		CapRef:      meta.Ref.String(),
		Description: meta.Description,
		Arguments:   argsJSON,
	}

	// 支持决策记忆的通道:多出"总是允许/拒绝"档
	if di, ok := it.(DecisionInteractor); ok && st != nil {
		d, err := di.ApproveDecision(ctx, req)
		if err != nil {
			return fmt.Sprintf("操作未执行:批准流程失败(%v)。", err), nil
		}
		switch d {
		case DecisionAlwaysAllow:
			st.memorize(ctx, meta.Ref.Key(), true)
			return exec(ctx)
		case DecisionAllow:
			return exec(ctx)
		case DecisionAlwaysDeny:
			st.memorize(ctx, meta.Ref.Key(), false)
		}
		return fmt.Sprintf("操作未执行:用户拒绝了 %s 的本次调用。请调整方案或询问用户意图。", meta.Ref.Name), nil
	}

	ok, err := it.Approve(ctx, req)
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
