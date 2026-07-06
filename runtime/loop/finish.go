package loop

import (
	"context"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// FinishGuard 是"收口守卫"(Ring 0):拦截两类把回合带进死胡同的最终文本,
// 注入纠正指令弹回模型重试(有界),不靠模型自觉。
//
//  1. 伪工具调用:模型把调用写成文本/代码块(functions.todo_write({...})、
//     <tool_call> 标记等)而没有发起真实 tool_call——react 循环会把它当
//     最终回答收尾,任务原地中断;
//  2. 空头承诺:纯文本以"请稍等/我将继续执行"收尾——文本即终点,
//     不存在"稍后",回合结束后没有任何东西会继续执行。
//
// 弹回是同一次 Generate 内的重试(每次调用最多 maxBounces 次),经内层
// Budget/Retry 计费与重试;仍不合格则原样放行(守卫是纠偏,不是硬闸)。
// 只作用于 Generate:流式路径直出用户,不适合弹回。
func FinishGuard(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	return &finishGuard{inner: m}
}

const finishGuardBounces = 2

// 伪调用形态:functions.xxx( 代码调用体、<tool_call> 标记、"tool_calls" 键,
// 以及裸 JSON 工具载荷(把 todo_write 的参数直接写进正文代码块,如
// ```json {"todos": [...]}```——实测 MiniMax 的高频变体)。
var pseudoCallRe = regexp.MustCompile(`(?s)(functions|tools|multi_tool_use)\.[a-zA-Z_][\w.-]*\s*\(|<tool_call>|"tool_calls"\s*:|"todos"\s*:\s*\[`)

// pseudoPlanRe 抓"叙述式执行":正文把任务状态写成机器状态词(状态: pending /
// status: in_progress),通常整段是一份"看起来在执行"的计划文档,实际零调用
// (实测变体:零工具调用的轮次输出五步计划,每步配参数 JSON 和 in_progress)。
var pseudoPlanRe = regexp.MustCompile("(?i)(状态|status)\\s*[::]\\s*[`'\"]?(pending|in_progress)")

// emptyPromises 是"承诺后续执行"的收尾话术(纯文本终局时它们必然落空)。
// 英文变体按小写匹配(L1 为英文后模型可能以英文承诺)。
var emptyPromises = []string{
	"请稍等", "稍等片刻", "我将继续", "我会继续", "接下来我将", "接下来我会",
	"正在为您处理", "马上为您", "请等待",
}

var emptyPromisesEN = []string{
	"i'll continue", "i will continue", "please wait", "one moment",
	"i'll now proceed", "i will now proceed", "let me continue", "hang on while i",
}

// badFinal 判定一条无 tool_calls 的最终文本是否该弹回。
func badFinal(content string) (reason string, bad bool) {
	if pseudoCallRe.MatchString(content) {
		return "输出里出现了文本形式的工具调用——那只是字符串,不会被执行", true
	}
	if pseudoPlanRe.MatchString(content) {
		return "输出把任务状态写成了正文(pending/in_progress)——计划必须用 todo_write 真实登记,每一步执行必须发起真实的工具调用,文字叙述不会执行任何东西", true
	}
	for _, p := range emptyPromises {
		if strings.Contains(content, p) {
			return "输出以「" + p + "」收尾——回合结束后不存在任何'稍后',这句承诺必然落空", true
		}
	}
	lower := strings.ToLower(content)
	for _, p := range emptyPromisesEN {
		if strings.Contains(lower, p) {
			return "输出以「" + p + "」收尾——回合结束后不存在任何'稍后',这句承诺必然落空", true
		}
	}
	return "", false
}

type finishGuard struct {
	inner model.ToolCallingChatModel
}

func (g *finishGuard) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	out, err := g.inner.Generate(ctx, msgs, opts...)
	for bounce := 0; err == nil && len(out.ToolCalls) == 0 && bounce < finishGuardBounces; bounce++ {
		reason, bad := badFinal(out.Content)
		if !bad {
			break
		}
		msgs = append(msgs, out, schema.SystemMessage(
			"[收口检查] 上一条输出无效:"+reason+"。"+
				"要继续执行任务,必须现在就发起真实的工具调用(tool_call);"+
				"任务已完成就直接给出最终结果;确实无法完成就说明原因并更新计划。不要输出代码块形式的调用,不要承诺稍后。"))
		out, err = observedGenerate(ctx, "finish-guard/bounce", func(ctx context.Context, ms []*schema.Message) (*schema.Message, error) {
			return g.inner.Generate(ctx, ms, opts...)
		}, msgs)
	}
	// 弹回预算耗尽仍是伪执行形态:放行但打上显式标记——编造的"执行结果"
	// 不允许冒充真实执行(实测退化形态:弹回后模型反而虚构 completed 与
	// 数据;守卫是纠偏不是硬闸,但诚实性必须兜底)。
	if err == nil && len(out.ToolCalls) == 0 {
		if _, bad := badFinal(out.Content); bad {
			annotated := *out
			annotated.Content = "[系统提示] 本轮未执行任何真实的工具调用,以下内容由模型直接生成、未经业务数据验证,请谨慎采信。\n\n" + out.Content
			return &annotated, nil
		}
	}
	return out, err
}

func (g *finishGuard) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return g.inner.Stream(ctx, msgs, opts...)
}

func (g *finishGuard) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := g.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &finishGuard{inner: inner}, nil
}

// CheckedFinish 给模型套上可插拔的收口检查(Ring 0):模型给出无 tool_calls
// 的最终文本时依次运行注入的检查,任一返回非空即作为纠正指令弹回重试
// (同 FinishGuard 的有界弹回)。与 FinishGuard 的分工:FinishGuard 拦
// "输出形态无效"(伪调用/空头承诺),内置且无状态;CheckedFinish 挂
// "收尾时状态未收口"类业务纪律(如 todo 计划残留),检查由装配层注入,
// loop 不感知具体纪律。检查自身负责节流(如"每轮最多催一次",经
// runctx.TurnState 去重),守卫只管弹回。只作用于 Generate,流式透传。
func CheckedFinish(m model.ToolCallingChatModel, checks ...func(context.Context) string) model.ToolCallingChatModel {
	if len(checks) == 0 {
		return m
	}
	return &checkedFinish{inner: m, checks: checks}
}

type checkedFinish struct {
	inner  model.ToolCallingChatModel
	checks []func(context.Context) string
}

func (g *checkedFinish) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	out, err := g.inner.Generate(ctx, msgs, opts...)
	for bounce := 0; err == nil && len(out.ToolCalls) == 0 && bounce < finishGuardBounces; bounce++ {
		correction := ""
		for _, check := range g.checks {
			if correction = check(ctx); correction != "" {
				break
			}
		}
		if correction == "" {
			break
		}
		msgs = append(msgs, out, schema.SystemMessage(correction))
		out, err = observedGenerate(ctx, "finish-check/bounce", func(ctx context.Context, ms []*schema.Message) (*schema.Message, error) {
			return g.inner.Generate(ctx, ms, opts...)
		}, msgs)
	}
	return out, err
}

func (g *checkedFinish) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return g.inner.Stream(ctx, msgs, opts...)
}

func (g *checkedFinish) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := g.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &checkedFinish{inner: inner, checks: g.checks}, nil
}
