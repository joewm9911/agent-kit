package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
)

// TestFinishGuardPseudoToolCall:文本形式的工具调用被弹回,模型改发
// 真实 tool_call 后放行;纠正指令随消息注入。
func TestFinishGuardPseudoToolCall(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("```typescript\nfunctions.todo_write({\"todos\": []})\n```", nil),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "todo_write", Arguments: "{}"}}}),
	)
	g := FinishGuard(m)
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("做任务")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("guard should bounce pseudo-call and surface the real tool call, got %+v", out)
	}
	if m.Calls != 2 {
		t.Fatalf("calls = %d, want 2(一次原始 + 一次弹回)", m.Calls)
	}
}

// TestFinishGuardEmptyPromise:"请稍等,我将继续执行"被弹回;第二次给出
// 真实终局文本后放行。
func TestFinishGuardEmptyPromise(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("好的,请稍等,我将继续执行这些任务。", nil),
		schema.AssistantMessage("已全部完成:共 1 款产品,合计 129 元。", nil),
	)
	g := FinishGuard(m)
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("汇总")})
	if err != nil || !strings.Contains(out.Content, "已全部完成") {
		t.Fatalf("got %q %v", out.Content, err)
	}
}

// TestFinishGuardBounceCap:连续不合格最多弹回 2 次,之后原样放行
// (守卫是纠偏不是硬闸,不能造成死循环)。
func TestFinishGuardBounceCap(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("请稍等。", nil),
		schema.AssistantMessage("请稍等。", nil),
		schema.AssistantMessage("请稍等。", nil),
	)
	g := FinishGuard(m)
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Calls != 3 { // 1 原始 + 2 弹回
		t.Fatalf("calls = %d, want 3", m.Calls)
	}
	if !strings.Contains(out.Content, "请稍等") {
		t.Fatalf("exhausted guard must pass through as-is, got %q", out.Content)
	}
}

// TestFinishGuardPassThrough:正常终局文本与真实工具调用零干预。
func TestFinishGuardPassThrough(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("降噪耳机 129 元。", nil))
	g := FinishGuard(m)
	out, _ := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("查价")})
	if out.Content != "降噪耳机 129 元。" || m.Calls != 1 {
		t.Fatalf("normal final must pass untouched: %q calls=%d", out.Content, m.Calls)
	}

	m2 := testmodel.New(schema.AssistantMessage("我将继续查询", []schema.ToolCall{{ID: "c", Type: "function",
		Function: schema.FunctionCall{Name: "search", Arguments: "{}"}}}))
	g2 := FinishGuard(m2)
	out2, _ := g2.Generate(context.Background(), []*schema.Message{schema.UserMessage("查")})
	if len(out2.ToolCalls) != 1 || m2.Calls != 1 {
		t.Fatal("messages with real tool calls must never bounce(带调用的'我将继续'是真的)")
	}
}

// TestCheckedFinish:注入的收口检查返回纠正 → 弹回重试;检查放行后直出;
// 无检查时原样返回底模。
func TestCheckedFinish(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("做完了。", nil),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "todo_write", Arguments: "{}"}}}),
	)
	nags := 0
	check := func(context.Context) string {
		if nags == 0 { // 自节流:只催一次(对齐 todo.FinishCheck 的每轮一次)
			nags++
			return "[计划收口] 先更新清单再收尾。"
		}
		return ""
	}
	g := CheckedFinish(m, check)
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("做任务")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("bounce should surface the reconciling tool call, got %+v", out)
	}
	if m.Calls != 2 || nags != 1 {
		t.Fatalf("calls=%d nags=%d, want 2/1", m.Calls, nags)
	}

	// 检查全程放行:一次直出
	m2 := testmodel.New(schema.AssistantMessage("回答。", nil))
	out, err = CheckedFinish(m2, func(context.Context) string { return "" }).
		Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "回答。" || m2.Calls != 1 {
		t.Fatalf("pass-through failed: %v %+v calls=%d", err, out, m2.Calls)
	}

	// 无检查:返回原模型
	m3 := testmodel.New(schema.AssistantMessage("x", nil))
	if CheckedFinish(m3) != m3 {
		t.Fatal("no checks should return the model unchanged")
	}
}

// TestCheckedFinishStubborn:模型顶着纠正仍出纯文本 → 有界弹回后放行,
// 不死循环(检查不自节流时由 finishGuardBounces 兜底)。
func TestCheckedFinishStubborn(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("就这样。", nil),
		schema.AssistantMessage("还是这样。", nil),
		schema.AssistantMessage("不改。", nil),
	)
	g := CheckedFinish(m, func(context.Context) string { return "收口!" })
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "不改。" || m.Calls != 3 {
		t.Fatalf("bounded bounce: content=%q calls=%d, want 不改。/3", out.Content, m.Calls)
	}
}

// TestFinishGuardBareJSONTodos:把 todo_write 参数写进正文代码块的伪调用
// (```json {"todos": [...]}```,MiniMax 实测高频变体)被识别并弹回。
func TestFinishGuardBareJSONTodos(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("### 执行步骤\n```json\n{\n  \"todos\": [\n    {\"content\": \"查商品\", \"status\": \"pending\"}\n  ]\n}\n```\n请确认是否执行。", nil),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "todo_write", Arguments: `{"todos":[{"content":"查商品","status":"pending"}]}`}}}),
	)
	g := FinishGuard(m)
	out, err := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("先列计划再动手")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("bare-JSON todos must bounce into a real tool call, got %+v", out)
	}
	if m.Calls != 2 {
		t.Fatalf("calls = %d, want 2", m.Calls)
	}
}

// TestFinishGuardNarratedPlan:整轮零调用、正文叙述"状态: pending/in_progress"
// 的计划文档(叙述式执行)被识别弹回。
func TestFinishGuardNarratedPlan(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("### 任务计划\n1. 生成销售报表\n   - 状态: `pending`\n2. 识别亏本商品\n   - 状态: in_progress", nil),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "todo_write", Arguments: `{"todos":[{"content":"生成销售报表","status":"pending"}]}`}}}),
	)
	out, err := FinishGuard(m).Generate(context.Background(), []*schema.Message{schema.UserMessage("先列计划再动手")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 1 || m.Calls != 2 {
		t.Fatalf("narrated plan must bounce into real calls, got %+v calls=%d", out, m.Calls)
	}
}

// TestFinishGuardHonestyMark:弹回预算耗尽仍是伪执行 → 放行但打免责标记,
// 编造内容不冒充真实执行。
func TestFinishGuardHonestyMark(t *testing.T) {
	stubborn := schema.AssistantMessage("1. 生成报表 - 状态: in_progress\n2. 识别亏本 - 状态: pending", nil)
	m := testmodel.New(stubborn, stubborn, stubborn)
	out, err := FinishGuard(m).Generate(context.Background(), []*schema.Message{schema.UserMessage("动手")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Content, "[系统提示]") || m.Calls != 3 {
		t.Fatalf("stubborn pseudo-plan must be annotated, got %q calls=%d", out.Content[:30], m.Calls)
	}
}

// TestFinishGuardEnglishPromise:英文空头承诺(L1 英化后的新形态)同样弹回。
func TestFinishGuardEnglishPromise(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("Please wait, I'll continue with the analysis.", nil),
		schema.AssistantMessage("最终结论:P100 定价合理。", nil),
	)
	out, err := FinishGuard(m).Generate(context.Background(), []*schema.Message{schema.UserMessage("分析")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "最终结论") || m.Calls != 2 {
		t.Fatalf("english empty promise must bounce: %q calls=%d", out.Content, m.Calls)
	}
}

// TestFinishGuardNarratedSteps:无状态词的"计划/执行步骤"文档变体(实测
// 用户连问两次得到逐字相同的零调用计划)被识别弹回;单独的"后续步骤"
// 建议或单独提及工具名不误伤。
func TestFinishGuardNarratedSteps(t *testing.T) {
	narrated := "### 重新分析计划:\n1. **查询订单总量**:\n   - 使用 `order-inquiry` 工具查询。\n\n### 执行步骤:\n1. 调用 `order-inquiry` 工具查询订单总量。\n2. 调用 `check-inventory` 工具检查库存。"
	m := testmodel.New(
		schema.AssistantMessage(narrated, nil),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "order-inquiry", Arguments: `{"task":"查询订单总量"}`}}}),
	)
	out, err := FinishGuard(m).Generate(context.Background(), []*schema.Message{schema.UserMessage("重新分析一遍")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 1 || m.Calls != 2 {
		t.Fatalf("narrated steps doc must bounce into real calls: %+v calls=%d", out, m.Calls)
	}

	// 不误伤:①"后续步骤"标题但没有"调用 X 工具"句式;②有句式但没有计划标题
	ok1 := schema.AssistantMessage("### 分析结论\n库存充足。\n\n### 后续步骤\n- 定期监控库存水平。", nil)
	ok2 := schema.AssistantMessage("查不到该口径,建议使用 check-inventory 工具查询单品库存。", nil)
	for i, msg := range []*schema.Message{ok1, ok2} {
		mm := testmodel.New(msg)
		out, err := FinishGuard(mm).Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
		if err != nil || out.Content != msg.Content || mm.Calls != 1 {
			t.Fatalf("case %d must pass untouched: %v %q calls=%d", i, err, out.Content, mm.Calls)
		}
	}
}

// TestPureCompletionNotice 是"纯完成状态"守卫的规格:该拦的元陈述空壳 vs
// 该放行的合法答复(含数字/具体动作/实质结论/带真实内容的完成句)。
func TestPureCompletionNotice(t *testing.T) {
	block := []string{
		"任务已全部完成。",                         // 实测复现的原样
		"任务已全部完成。如有其他需求请告诉我。",   // 带礼貌语变体
		"分析方案已完成",                           // 用户报的原话
		"任务完成。",
		"计划已执行完毕。",
		"全部任务已完成,请查收。",
		"All tasks completed.",
		"The task is complete.",
		// —— 真机 A/B 实测漏抓的变体(第一轮守卫正则的盲区)——
		"所有任务已完成。如有其他问题请告诉我。",
		"所有步骤已完成。如有其他问题，请随时告诉我。",
		"所有步骤已完成，分析结论已给出。如有其他问题请继续吩咐。",
		"所有步骤已完成，结论已给出。如需其他操作请告知。",
		"所有任务已完成。如需进一步分析（如查询销量、调整建议等），请告知。",
		// Ark 生产实测:指涉性空壳——交付物在循环中间消息里,终答只说"已输出"
		"分析方案已输出，可执行取数。",
		"结论见上方。",
	}
	for _, s := range block {
		if !pureCompletionNotice(s) {
			t.Errorf("应拦(纯完成状态): %q", s)
		}
	}

	pass := []string{
		"补货已完成,库存更新为 92 件。",           // 动作确认:有数字
		"已下架 P100。",                            // 动作确认:具体对象、无完成元词
		"降噪耳机 129 元。",                         // 纯数据
		"分析完成,建议补货:近30天销量增长明显。", // 完成句 + 实质结论
		"任务已完成:P100 库存 42、售价 199、退款 3 笔。", // 完成句 + 数据
		"暂不需要补货,库存充足。",                 // 结论、无完成元词
		"42 件",
		// 真机 run3 变体:有完成元词但带出了实质结论 → 放行(守卫只拦纯空壳)
		"所有任务已完成。P100 商品的综合分析结论：**暂不需要补货**，理由是库存充足且退款率低。",
		// politeRe 曾因第二组可选而过度剥除,把下面两条实质内容误判为空壳(实测)
		"分析完成,如果按品类看主要是耳机拉动。",
		"任务完成,还差物流数据没查到。",
		// 肯定/否定应答:回答"完成了吗"的正当答案,不是拿状态顶替交付物
		"是的,全部任务已完成。",
		"还没,任务尚未全部完成。",
		"Yes, the task is complete.",
	}
	for _, s := range pass {
		if pureCompletionNotice(s) {
			t.Errorf("误伤(该放行): %q", s)
		}
	}
}

// TestFinishGuardCompletionNotice:端到端——空壳完成状态弹回,模型补出实质
// 内容后放行;开关关闭时不介入(A/B 对照的确定性验证)。
func TestFinishGuardCompletionNotice(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("任务已全部完成。", nil),                       // 空壳 → 弹回
		schema.AssistantMessage("P100 库存 42 件、售价 199 元、退款 3 笔。", nil), // 补出内容 → 放行
	)
	g := FinishGuard(m)
	out, _ := g.Generate(context.Background(), []*schema.Message{schema.UserMessage("分步查清并给结论")})
	if !strings.Contains(out.Content, "42") || m.Calls != 2 {
		t.Fatalf("空壳完成状态应弹回并补内容: %q calls=%d", out.Content, m.Calls)
	}

	// 开关关闭:空壳原样放行(证明拦截确由此守卫贡献)
	CompletionNoticeGuard = false
	defer func() { CompletionNoticeGuard = true }()
	m2 := testmodel.New(schema.AssistantMessage("任务已全部完成。", nil))
	g2 := FinishGuard(m2)
	out2, _ := g2.Generate(context.Background(), []*schema.Message{schema.UserMessage("查")})
	if out2.Content != "任务已全部完成。" || m2.Calls != 1 {
		t.Fatalf("关闭守卫后应原样放行: %q calls=%d", out2.Content, m2.Calls)
	}
}

// TestFinishGuardSplicesPriorDeliverable(Ark 轨迹回归):模型顶着弹回连出
// 指涉性空壳("方案已输出")时,真交付物就在循环中间消息里——harness 直接
// 拼接它收口(组件返回值只取最后一条,不拼接调用方就永远看不到)。
func TestFinishGuardSplicesPriorDeliverable(t *testing.T) {
	deliverable := "## CBT渗透率分析方案\n维度:地区/品类/时间;口径:近30天;步骤:先取分母口径再取分子,输出对比表与结论建议。"
	shell := schema.AssistantMessage("分析方案已输出，可执行取数。", nil)
	m := testmodel.New(shell, shell, shell) // 弹回两次仍空壳 → Force
	g := FinishGuard(m)
	history := []*schema.Message{
		schema.UserMessage("输出完整分析方案"),
		schema.AssistantMessage(deliverable, nil), // ← 循环内的真产出
		schema.SystemMessage("[执行记录] todo_write 已全部完成"),
	}
	out, err := g.Generate(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "CBT渗透率分析方案") {
		t.Fatalf("harness must splice the prior deliverable, got %q", out.Content)
	}
	if strings.Contains(out.Content, "[系统提示]") {
		t.Fatal("spliced final must not carry the false annotation")
	}
	if m.Calls != 3 {
		t.Fatalf("calls = %d, want 3 (原始 + 两次弹回)", m.Calls)
	}

	// 历史里没有实质产出:维持原 Force 标注路径(拼无可拼)
	m2 := testmodel.New(shell, shell, shell)
	out2, _ := FinishGuard(m2).Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("干活")})
	if !strings.HasPrefix(out2.Content, "[系统提示]") {
		t.Fatalf("no prior deliverable → keep annotation, got %q", out2.Content)
	}
}
