package loop

import (
	"context"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/model"
)

// FinishGuard 是"收口守卫"的兼容外观:= 单评审器的 ReviewModel(预算
// 沿旧值 finishGuardBounces,行为与旧实现逐位一致——既有行为测试是
// 迁移验收线)。组合装配请直接用 ReviewModel(评审器列表 + 全局预算),
// 见 docs/review-model-design.md。
func FinishGuard(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	return reviewModelN(m, finishGuardBounces, FinishReviewer())
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

// 叙述式执行的无状态词变体(实测连续复发):终答是一份"计划/执行步骤"
// 文档,逐条写着"调用/使用 `X` 工具",但整段没有任何真实 tool_call——
// 两个指纹同时命中才判定(单独的"后续步骤"建议或单独提及工具名不拦)。
var (
	narratedPlanHeadRe = regexp.MustCompile("(?m)^#+\\s*[^\n]{0,16}(计划|执行步骤)")
	narratedCallRe     = regexp.MustCompile("(调用|使用)\\s*[`'\"]?[a-zA-Z][\\w-]*[`'\"]?\\s*(工具|技能)")
)

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
	if narratedPlanHeadRe.MatchString(content) && narratedCallRe.MatchString(content) {
		return "输出是一份'计划/执行步骤'文档,写着要调用哪些工具却没有发起任何真实调用——文字不会执行任何东西。现在就发起第一步的真实 tool_call 开始执行", true
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

// CheckedFinish 是可插拔收口检查的兼容外观:= 单评审器的 ReviewModel。
// 检查由装配层注入并自行节流(经 runctx.TurnState);无检查时原样返回。
func CheckedFinish(m model.ToolCallingChatModel, checks ...func(context.Context) string) model.ToolCallingChatModel {
	if len(checks) == 0 {
		return m
	}
	return reviewModelN(m, finishGuardBounces, CheckedReviewer(checks...))
}
