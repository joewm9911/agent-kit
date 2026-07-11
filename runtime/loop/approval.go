package loop

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"strings"
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

	// 决策阶段与执行阶段分离:锁(弹窗串行化 + 锁内重查)只罩决策——
	// 模型并行发起一批同能力调用时,全部 goroutine 会在任何弹窗被回答之前
	// 越过外层 recall 检查,锁内重查让用户第一次"总是允许"救下已排队的
	// 其余弹窗(实测 11 连弹)。执行必须在锁外:mutating skill 的内层工具
	// 再触同一 ApprovalState 时,锁跨执行 = 自死锁;即使不嵌套,一个长工具
	// 也会把全会话的审批串行在它后面。
	allowed, err := func() (bool, error) {
		if st != nil {
			st.promptSerialize()
			defer st.promptRelease()
			if a, ok := st.recall(ctx, meta.Ref.Key()); ok {
				return a, nil
			}
		}
		// 支持决策记忆的通道:多出"总是允许/拒绝"档
		if di, ok := it.(DecisionInteractor); ok && st != nil {
			d, derr := di.ApproveDecision(ctx, req)
			if derr != nil {
				return false, derr
			}
			switch d {
			case DecisionAlwaysAllow:
				st.memorize(ctx, meta.Ref.Key(), true)
				return true, nil
			case DecisionAllow:
				return true, nil
			case DecisionAlwaysDeny:
				st.memorize(ctx, meta.Ref.Key(), false)
			}
			return false, nil
		}
		return it.Approve(ctx, req)
	}()
	if err != nil {
		// 轮次终止级错误(挂起 ErrSuspended/中断/预算硬停)必须穿透——
		// 转成结果字符串会让挂起模式下的审批"问了却不挂起":用户收到审批
		// 问题,轮次却没挂起,后续的"同意"变成一条全新输入(功能性坏死)。
		if turnTerminalErr(err) {
			return "", err
		}
		return fmt.Sprintf("操作未执行:批准流程失败(%v)。", err), nil
	}
	if !allowed {
		return deniedResult(ctx, meta.Ref.Name), nil
	}
	return exec(ctx)
}

// deniedResult 生成"用户拒绝"的结果,并把拒绝记入轮状态袋——收口检查
// (DeniedCallsCheck)据此强制终答如实区分已执行与被拒绝。
func deniedResult(ctx context.Context, name string) string {
	if bag := runctx.TurnState(ctx); bag != nil {
		key := deniedCallsKey + runctx.Scope(ctx)
		v, _ := bag.Load(key)
		names, _ := v.([]string)
		bag.Store(key, append(names, name))
	}
	return fmt.Sprintf("操作未执行:用户拒绝了 %s 的本次调用。请调整方案或询问用户意图。", name)
}

const deniedCallsKey = "loop.denied\x1f"

// DeniedCallsCheck 是收口检查(经 CheckedFinish 注入):本轮存在被用户
// 拒绝的调用时,弹回一次要求终答如实区分已执行与被拒绝的操作——实测
// 模型会把被拒的调用也标成"已完成"(诚实性,Ring 0 兜底)。每轮最多一次。
func DeniedCallsCheck(ctx context.Context) string {
	bag := runctx.TurnState(ctx)
	if bag == nil {
		return ""
	}
	key := deniedCallsKey + runctx.Scope(ctx)
	v, _ := bag.Load(key)
	names, _ := v.([]string)
	if len(names) == 0 {
		return ""
	}
	if _, nagged := bag.Load(key + "\x1fnagged"); nagged {
		return ""
	}
	bag.Store(key+"\x1fnagged", true)
	return fmt.Sprintf("[收口检查] 本轮有 %d 个调用被用户拒绝、并未执行(%s)。最终回答必须如实区分:哪些操作真正执行成功、哪些被拒绝未执行,不得声称全部完成。", len(names), strings.Join(names, "、"))
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
