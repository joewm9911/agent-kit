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
