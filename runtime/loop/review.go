// review.go:生成评审统一层(Ring 0)。借 eino ADK ShouldRetry 的洞察——
// 质量否决与重试是同一个接缝——把伪调用/空头承诺/计划收口/重复调用等
// 守卫收敛为"一个有界评审循环 + 有序评审器列表":
//
//	尝试生成 → 依序征询 Reviewer(首个非 Accept 生效)
//	  Accept → 放行;Force → 以 Replace 收束(不再评审);
//	  Retry  → Append 纠正 / Rewrite 改写输入,受全局预算与退避约束。
//
// 全局重试预算取代旧守卫链各自计数的乘法放大(2×2×2 无全局闸);
// 顺序从包装嵌套的隐式约定变为显式列表;新增守卫 = 写一个纯函数。
// 守卫是纠偏不是硬闸:预算耗尽后放行最后一次输出。
// 轮次终止级错误(TurnTerminal/HITL 中断)直接透传,不进评审——
// 那是给框架看的信号,不是给模型看的质量问题。
// 设计全文见 docs/review-model-design.md。
package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Attempt 是一次模型生成的完整上下文,交给评审器裁决。
type Attempt struct {
	N    int               // 第几次尝试(1 起)
	Msgs []*schema.Message // 本次实际发送的消息
	Out  *schema.Message   // Err == nil 时有效
	Err  error

	tally map[string]int
}

// Tally 返回同名 Reason 的评审器此前已触发的次数——评审器据此自限
// (如"最多纠正 2 次,之后强制收束"),自身保持纯函数。
func (a Attempt) Tally(reason string) int { return a.tally[reason] }

// VerdictAction 是评审结论的动作。零值 Accept:未触发即放行。
type VerdictAction int

const (
	Accept VerdictAction = iota // 放行
	Retry                       // 重试(Append/Rewrite 之一,+可选 Backoff)
	Force                       // 强制收束:Replace 为最终输出,不再评审
)

// Verdict 是一次评审的结论。
type Verdict struct {
	Action VerdictAction
	// Reason 是触发计数键 + 观测 span 名(review/<reason>),必填于非 Accept。
	Reason string
	// Append 追加进下次尝试的消息(Retry;弹回纠正的标准形态)。
	Append []*schema.Message
	// Rewrite 非空时整体替换下次尝试的消息(Retry;改写输入重试,
	// 如 429 裁剪/上下文超限截短)。与 Append 二选一,Rewrite 优先。
	Rewrite []*schema.Message
	// Replace 是 Force 的最终输出。
	Replace *schema.Message
	// Backoff 是重试前等待(0 = 不等)。
	Backoff time.Duration
}

// Reviewer 是一个生成质量评审器:纯函数,轮内状态经 runctx.TurnState、
// 自限计数经 Attempt.Tally,不持有可变字段。
type Reviewer func(ctx context.Context, a Attempt) Verdict

// reviewMaxRetries 是每次逻辑调用的全局评审重试预算(不含首次生成;
// 与内层 RetryModel 的瞬时错误重试无关——那层同参不计费,这层经
// BudgetModel 计费)。
const reviewMaxRetries = 4

// ReviewModel 给模型套上评审循环(全局预算 reviewMaxRetries)。
// 无评审器时原样返回。
func ReviewModel(m model.ToolCallingChatModel, rs ...Reviewer) model.ToolCallingChatModel {
	return reviewModelN(m, reviewMaxRetries, rs...)
}

// reviewModelN 是带自定义预算的构造器:兼容外观(FinishGuard 等)用旧
// 守卫的独立预算保持行为原样;组合装配用全局预算。
func reviewModelN(m model.ToolCallingChatModel, maxRetries int, rs ...Reviewer) model.ToolCallingChatModel {
	if len(rs) == 0 {
		return m
	}
	return &reviewedModel{inner: m, reviewers: rs, maxRetries: maxRetries}
}

type reviewedModel struct {
	inner      model.ToolCallingChatModel
	reviewers  []Reviewer
	maxRetries int
}

func (r *reviewedModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	tally := map[string]int{}
	out, err := r.inner.Generate(ctx, msgs, opts...)

	for retries := 0; retries < r.maxRetries; retries++ {
		if err != nil && turnTerminalErr(err) {
			return out, err // 轮次终止级信号透传,不进评审
		}
		a := Attempt{N: retries + 1, Msgs: msgs, Out: out, Err: err, tally: tally}
		v := Verdict{}
		for _, review := range r.reviewers {
			if v = review(ctx, a); v.Action != Accept {
				break
			}
		}
		switch v.Action {
		case Accept:
			return out, err
		case Force:
			return v.Replace, nil
		}
		// Retry
		tally[v.Reason]++
		if v.Backoff > 0 {
			select {
			case <-time.After(v.Backoff):
			case <-ctx.Done():
				return out, err
			}
		}
		if len(v.Rewrite) > 0 {
			msgs = v.Rewrite
		} else {
			msgs = append(msgs, v.Append...)
		}
		out, err = observedGenerate(ctx, "review/"+v.Reason,
			func(ctx context.Context, ms []*schema.Message) (*schema.Message, error) {
				return r.inner.Generate(ctx, ms, opts...)
			}, msgs)
	}
	// 预算耗尽:给评审器最后一次"收束或放行"的机会(只接受 Force——
	// 诚实标记类兜底靠它;再给 Retry 也没有预算了)。
	if err == nil {
		a := Attempt{N: r.maxRetries + 1, Msgs: msgs, Out: out, Err: err, tally: tally}
		for _, review := range r.reviewers {
			if v := review(ctx, a); v.Action == Force {
				return v.Replace, nil
			} else if v.Action != Accept {
				break // Retry 已无预算:放行
			}
		}
	}
	return out, err
}

// Stream 透传:流式路径直出用户,不适合弹回(与旧守卫一致)。
func (r *reviewedModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return r.inner.Stream(ctx, msgs, opts...)
}

func (r *reviewedModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := r.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &reviewedModel{inner: inner, reviewers: r.reviewers, maxRetries: r.maxRetries}, nil
}

// turnTerminalErr 与 engine 侧 turnTerminal 同判据(engine 不能依赖 loop,
// 两处各持一份小函数;标记接口是共同契约)。
func turnTerminalErr(err error) bool {
	var tt interface{ TurnTerminal() }
	return errors.As(err, &tt)
}

// ---- 三个内置评审器(判定逻辑与纠正文案沿用原守卫,零改动) ----

const (
	reasonFinish      = "finish-guard"
	reasonFinishCheck = "finish-check"
	reasonRepeatBreak = "repeat-break"
	finishSelfLimit   = 2 // FinishReviewer 自限:纠正 2 次后走诚实标记
)

// FinishReviewer 迁移自 FinishGuard:拦伪调用/空头承诺/叙述式执行;
// 自限耗尽仍不合格时 Force 打诚实标记(编造不冒充真实执行)。
func FinishReviewer() Reviewer {
	return func(_ context.Context, a Attempt) Verdict {
		if a.Err != nil || a.Out == nil || len(a.Out.ToolCalls) > 0 {
			return Verdict{}
		}
		reason, bad := badFinal(a.Out.Content)
		if !bad {
			return Verdict{}
		}
		if a.Tally(reasonFinish) >= finishSelfLimit {
			annotated := *a.Out
			annotated.Content = "[系统提示] 本轮未执行任何真实的工具调用,以下内容由模型直接生成、未经业务数据验证,请谨慎采信。\n\n" + a.Out.Content
			return Verdict{Action: Force, Reason: reasonFinish, Replace: &annotated}
		}
		return Verdict{Action: Retry, Reason: reasonFinish, Append: []*schema.Message{
			a.Out, schema.SystemMessage(
				"[收口检查] 上一条输出无效:" + reason + "。" +
					"要继续执行任务,必须现在就发起真实的工具调用(tool_call);" +
					"任务已完成就直接给出最终结果;确实无法完成就说明原因并更新计划。不要输出代码块形式的调用,不要承诺稍后。"),
		}}
	}
}

// CheckedReviewer 迁移自 CheckedFinish:装配层注入的收口检查
// (todo 计划收口、被拒调用诚实区分等),检查自身负责轮内节流。
func CheckedReviewer(checks ...func(context.Context) string) Reviewer {
	return func(ctx context.Context, a Attempt) Verdict {
		if a.Err != nil || a.Out == nil || len(a.Out.ToolCalls) > 0 {
			return Verdict{}
		}
		for _, check := range checks {
			if correction := check(ctx); correction != "" {
				return Verdict{Action: Retry, Reason: reasonFinishCheck, Append: []*schema.Message{
					a.Out, schema.SystemMessage(correction),
				}}
			}
		}
		return Verdict{}
	}
}

// RepeatBreakReviewer 迁移自 RepeatBreak:模型对已拦截热点再犯时,
// 第 1 次以 tool 消息回填纠正弹回(协议合法),第 2 次强制收束引用
// 真实缓存结果作答。
func RepeatBreakReviewer() Reviewer {
	return func(ctx context.Context, a Attempt) Verdict {
		if a.Err != nil {
			return Verdict{}
		}
		hot := hotEntry(ctx)
		if !matchesHot(a.Out, hot) {
			return Verdict{}
		}
		if a.Tally(reasonRepeatBreak) >= 1 {
			return Verdict{Action: Force, Reason: reasonRepeatBreak, Replace: schema.AssistantMessage(fmt.Sprintf(
				"(系统已终止对 %s 的重复调用:相同参数已执行/拦截 %d 次,结果不变)\n该调用的实际结果:\n%s",
				hot.name, hot.count, clip(hot.last, 2000)), nil)}
		}
		correction := fmt.Sprintf(
			"[重复调用终止] %s 已用完全相同的参数调用 %d 次,执行已被系统封禁,结果不会再变:\n%s\n禁止再发起这个调用。现在就基于上述结果给出回答;确需其他信息,改变参数或改用其他能力。",
			hot.name, hot.count, clip(hot.last, 2000))
		appendMsgs := []*schema.Message{a.Out}
		for _, tc := range a.Out.ToolCalls {
			appendMsgs = append(appendMsgs, schema.ToolMessage(correction, tc.ID))
		}
		return Verdict{Action: Retry, Reason: reasonRepeatBreak, Append: appendMsgs}
	}
}
