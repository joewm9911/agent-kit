// dedup.go:重复调用断路器(Ring 0)。同一轮内,同一能力用完全相同的参数
// 反复调用是弱模型的高频退化形态(实测:同参 python 连打十几次、同参
// read_result 打转,直到 max steps 把轮次拖死)。L1 只祈使"报错不要同参
// 重试",对"成功但反复调"没有硬约束——这里补上:
//
//	第 2 次同参且结果与上次一致 → 照常执行,结果后附加提醒;
//	第 3 次起                   → 不再执行,回放上次结果并要求换路径。
//
// 计数按 (执行域, 能力, 参数) 分键,存轮内状态袋(runctx.TurnState),
// 轮结束即弃;参数一变即重置,结果有变化也重置(轮询类调用不受影响)。
// 无轮语义(未经 agent 入口装袋)不介入。
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

const dedupWarn = "\n\n[重复调用] 本次调用与上一次的参数、结果完全相同——不要再重复,基于已有结果推进任务;需要不同信息就改变参数。"

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
	if st.count > 0 && out == st.last {
		bag.Store(key, dedupState{count: st.count + 1, last: st.last})
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

// RepeatBreak 是重复调用的模型层终止器(Ring 0):dedup 断路器拦截回放
// 两次后模型仍发起同一调用(热点),说明它不读结果里的文字提醒——此时
// 在发起端掐断:先注入纠正弹回一次;仍再犯就强制收束,剥离调用、引用
// 真实缓存结果直接作答,不再让退化循环烧到 max steps。流式透传。
func RepeatBreak(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	return &repeatBreak{inner: m}
}

type repeatBreak struct {
	inner model.ToolCallingChatModel
}

func (g *repeatBreak) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	out, err := g.inner.Generate(ctx, msgs, opts...)
	hot := hotEntry(ctx)
	if err != nil || !matchesHot(out, hot) {
		return out, err
	}
	// 弹回一次:纠正以 tool 消息回填(带 call ID,协议合法——assistant 的
	// tool_call 后必须跟 tool 结果,厂商会校验),明说执行已被封禁。
	msgs = append(msgs, out)
	correction := fmt.Sprintf(
		"[重复调用终止] %s 已用完全相同的参数调用 %d 次,执行已被系统封禁,结果不会再变:\n%s\n禁止再发起这个调用。现在就基于上述结果给出回答;确需其他信息,改变参数或改用其他能力。",
		hot.name, hot.count, clip(hot.last, 2000))
	for _, tc := range out.ToolCalls {
		msgs = append(msgs, schema.ToolMessage(correction, tc.ID))
	}
	out, err = g.inner.Generate(ctx, msgs, opts...)
	if err != nil || !matchesHot(out, hot) {
		return out, err
	}
	// 纠正无效:强制收束,用真实结果替它作答,终止退化循环。
	return schema.AssistantMessage(fmt.Sprintf(
		"(系统已终止对 %s 的重复调用:相同参数已执行/拦截 %d 次,结果不变)\n该调用的实际结果:\n%s",
		hot.name, hot.count, clip(hot.last, 2000)), nil), nil
}

func (g *repeatBreak) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return g.inner.Stream(ctx, msgs, opts...)
}

func (g *repeatBreak) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := g.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &repeatBreak{inner: inner}, nil
}
