package loop

import (
	"context"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/model"
)

// CompletionNoticeGuard 控制"纯完成状态"收口拦截是否生效(真机 A/B 用开关做
// 对照)。默认开:实测 MiniMax 在多步 + todo 任务上约 1/3 概率把最终消息答成
// 一句"任务已全部完成"、零实质内容——这是把内容整体替换掉,不是多说一句。
var CompletionNoticeGuard = true

// 纯完成状态:终答只是一句"任务/方案/计划/分析…完成"之类元陈述,没有任何
// 实质内容。紧约束避免误伤动作确认("已补货,库存 92 件"含数字/具体对象)。
var (
	// 泛指任务对象 + 完成动词。要求主语是"任务/方案/计划…"这类元指代,
	// 从而放过"已下架 P100""已补货 92 件"这类具体动作确认。
	// (所有/步骤/结论已给出 为真机 A/B 抓到的实测变体。)
	completionNoticeRe = regexp.MustCompile(`(?i)(所有|全部)?(任务|方案|计划|分析|工作|流程|步骤|全部)[^。.,，、\n]{0,6}(完成|完毕|结束|办完)|(结论|结果)已(给出|输出)|all (tasks|steps)[^.\n]{0,8}(completed|done)|task[^.\n]{0,6}(is )?complete`)
	completionDigitRe  = regexp.MustCompile(`[0-9０-９]`)
	// 注意:第二组不可写成可选——否则"(如果|还)+任意20字"整段被剥,把
	// "分析完成,如果按品类看主要是耳机拉动"这类实质结论误判成空壳(实测误伤)。
	completionPoliteRe = regexp.MustCompile(`(?i)(如有|若有|如果|如需|还)[^。.\n]{0,20}(告诉我|联系我|需求|问题|需要|告知|吩咐)|请(告诉我|随时|告知|查收|继续吩咐)|随时(联系|告知)|if you need[^.\n]{0,20}|let me know[^.\n]{0,20}`)
	meaningfulCharRe   = regexp.MustCompile(`[\p{Han}\p{L}\p{N}]`)
)

// completionAnswerRe:肯定/否定应答开头——"是的,全部任务已完成"是对"跑完了吗"
// 这类提问的正当回答,不是模型拿状态顶替交付物,豁免(否则弹回两次后还会被
// Force 打上"未执行工具调用"的不实标注)。
var completionAnswerRe = regexp.MustCompile(`^\s*(是的|是,|是，|对,|对，|没有|还没|尚未|不,|不，|yes\b|no\b)`)

// pureCompletionNotice:去掉完成句式 + 礼貌语后,几乎不剩实质字符,即判为
// "纯完成通知"。三重护栏:①命中完成句式 ②不含任何数字 ③残余实质 ≤3 字;
// 另豁免肯定/否定应答开头(答"完成了吗"的合法答案)。
func pureCompletionNotice(content string) bool {
	if !completionNoticeRe.MatchString(content) || completionDigitRe.MatchString(content) {
		return false
	}
	if completionAnswerRe.MatchString(strings.ToLower(content)) {
		return false
	}
	rest := completionNoticeRe.ReplaceAllString(content, "")
	rest = completionPoliteRe.ReplaceAllString(rest, "")
	return len(meaningfulCharRe.FindAllString(rest, -1)) <= 3
}

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
	if CompletionNoticeGuard && pureCompletionNotice(content) {
		return "最终消息只有'任务已完成'这类完成状态、没有任何实质内容——把完整结果本身直接给出来(数据、结论、依据),不要用完成通知代替答案", true
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
