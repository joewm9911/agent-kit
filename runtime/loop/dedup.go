// dedup.go:重复调用断路器(Ring 0)。同一轮内,同一能力用完全相同的参数
// 反复调用是弱模型的高频退化形态(实测:同参 python 连打十几次、同参
// read_result 打转,直到 max steps 把轮次拖死)。L1 只祈使"报错不要同参
// 重试",对"成功但反复调"没有硬约束——这里补上:
//
//	第 2 次重复 → 照常执行,结果后附加提醒;
//	第 3 次起   → 不再执行,回放上次结果并要求换路径。
//
// 重复的判定分两档:纯工具 = 同参**且**同结果(结果确定,变化说明是
// 轮询类,重置计数);kind=skill = **仅同参**——技能结果是子循环模型
// 生成的自然语言,每次措辞都不同,按结果判定永不触发(实测同参 mutating
// 技能连执行 5 次),而同轮内同参重调技能几乎必然是退化。
//
// 计数按 (执行域, 能力, 参数) 分键,存轮内状态袋(runctx.TurnState),
// 轮结束即弃;参数一变即重置。无轮语义(未经 agent 入口装袋)不介入。
package loop

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

const (
	// dedupBlockAfter 是开始拦截的重复次数:同参且同结果满 2 次后,第 3 次
	// 起不再真正执行。
	dedupBlockAfter = 2
	// dedupHotAfter 是升级为"热点"的总次数:拦截回放两次仍再犯,说明模型
	// 不读结果里的文字提醒,交给模型层的 RepeatBreak 终止(文字劝不动的
	// 退化循环,只能在发起端掐断)。
	dedupHotAfter  = dedupBlockAfter + 2
	dedupKeyPrefix = "loop.dedup\x1f"
	dedupHotKey    = "loop.dedup.hot\x1f" // + scope → *dedupHot
)

const dedupWarn = "\n\n[重复调用] 本次调用与上一次的参数完全相同——不要再重复,基于已有结果推进任务;需要不同信息就改变参数。"

// DedupCalls 给能力集套上重复调用断路器。
func DedupCalls(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &deduped{inner: c})
	}
	return out
}

// dedupState 是一个 (执行域, 能力, 参数) 键的轮内计数。
type dedupState struct {
	count int    // 连续同参同结果的次数
	last  string // 上次的原始结果(拦截时回放)
}

type deduped struct {
	inner capability.Capability
}

func (d *deduped) Meta() capability.Meta { return d.inner.Meta() }

func (d *deduped) key(ctx context.Context, argsJSON string) string {
	return dedupKeyPrefix + runctx.Scope(ctx) + "\x1f" + d.inner.Meta().Ref.String() + "\x1f" + argsJSON
}

// invoke 是断路逻辑本体:exec 负责真正执行(AsTool/AsLambda 各自注入)。
func (d *deduped) invoke(ctx context.Context, argsJSON string, exec func() (string, error)) (string, error) {
	bag := runctx.TurnState(ctx)
	if bag == nil {
		return exec()
	}
	key := d.key(ctx, argsJSON)
	prev, _ := bag.Load(key)
	st, _ := prev.(dedupState)

	if st.count >= dedupBlockAfter {
		next := dedupState{count: st.count + 1, last: st.last}
		bag.Store(key, next)
		if next.count >= dedupHotAfter {
			// 升级热点:模型层 RepeatBreak 据此在发起端掐断这个调用。
			bag.Store(dedupHotKey+runctx.Scope(ctx), &dedupHot{
				name: d.inner.Meta().Ref.Name, args: argsJSON, last: st.last, count: next.count,
			})
		}
		return st.last + fmt.Sprintf(
			"\n\n[重复调用已拦截] 该调用已用相同参数执行 %d 次且结果不变,本次未执行,以上为上次结果的回放。禁止再次重复;基于该结果推进,或改变参数/换用其他能力。",
			st.count), nil
	}

	out, err := exec()
	if err != nil {
		return out, err
	}
	// kind=skill 仅同参判定(模型生成的结果天然不稳定);纯工具还需同结果
	// (结果变化 = 轮询类,重置)。
	if st.count > 0 && (d.inner.Meta().Ref.Kind == "skill" || out == st.last) {
		bag.Store(key, dedupState{count: st.count + 1, last: out})
		return out + dedupWarn, nil
	}
	// 首次调用,或结果发生变化(轮询类):重新计数。
	bag.Store(key, dedupState{count: 1, last: out})
	return out, nil
}

func (d *deduped) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := d.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", d.inner.Meta().Ref)
	}
	return &dedupedTool{d: d, inner: inv}, nil
}

func (d *deduped) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return d.invoke(ctx, argsJSON, func() (string, error) {
			return capability.Invoke(ctx, d.inner, argsJSON)
		})
	}), nil
}

type dedupedTool struct {
	d     *deduped
	inner tool.InvokableTool
}

func (t *dedupedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *dedupedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return t.d.invoke(ctx, argsJSON, func() (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}

// ---- 模型层终止器 ----

// dedupHot 是升级到模型层的热点重复调用。
type dedupHot struct {
	name  string // 工具名(模型视角)
	args  string // 重复的参数原文
	last  string // 真实结果(强制收束时引用)
	count int
}

// hotEntry 取当前执行域的热点(无则 nil)。
func hotEntry(ctx context.Context) *dedupHot {
	bag := runctx.TurnState(ctx)
	if bag == nil {
		return nil
	}
	v, _ := bag.Load(dedupHotKey + runctx.Scope(ctx))
	h, _ := v.(*dedupHot)
	return h
}

// matchesHot 报告输出是否**只**在发起热点调用(全部 tool_calls 都命中才算:
// 夹带其他合法调用时交给执行层,dedup 会替热点调用回放,不在此拦)。
func matchesHot(out *schema.Message, h *dedupHot) bool {
	if h == nil || out == nil || len(out.ToolCalls) == 0 {
		return false
	}
	for _, tc := range out.ToolCalls {
		if tc.Function.Name != h.name || tc.Function.Arguments != h.args {
			return false
		}
	}
	return true
}

// RepeatBreak 是重复调用模型层终止器的兼容外观:= 单评审器的
// ReviewModel。组合装配请直接用 ReviewModel(见 review.go)。
func RepeatBreak(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	return reviewModelN(m, finishGuardBounces, RepeatBreakReviewer())
}
