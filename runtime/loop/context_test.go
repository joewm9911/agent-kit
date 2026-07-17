package loop

// 上下文架构批1/2 的锁定测试:语义信封 + 系统前缀字节稳定。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/reminder"
)

// TestModifierStablePrefixAndEnvelopes:同一会话多次调用,首条系统消息
// (L1+persona+env)逐字节相等——计划与记忆的每轮变化只出现在尾部追加
// 的 reminder 信封里,不污染稳定前缀(prompt cache 的地基)。
func TestModifierStablePrefixAndEnvelopes(t *testing.T) {
	planState := "当前任务计划 v1"
	layers := PromptLayers{
		Loop:   DefaultLoopPromptNoTodo,
		Persona: "你是测试助手",
		Plan:   func(context.Context) string { return planState },
		Memories: func(context.Context) []string {
			return []string{"用户偏好简短汇报"}
		},
	}
	mod := layers.Modifier()
	ctx := runctx.WithTurnState(context.Background())
	base := []*schema.Message{schema.UserMessage("hi")}

	out1 := mod(ctx, base)
	planState = "当前任务计划 v2(变化)" // 动态内容变化……
	out2 := mod(ctx, base)

	// ……但首条系统消息(稳定前缀)逐字节不变。
	if out1[0].Content != out2[0].Content {
		t.Fatal("leading system message must be byte-stable across calls")
	}
	// 稳定前缀里不得混入计划/记忆(它们属于尾部 reminder)。
	if strings.Contains(out1[0].Content, "任务计划") || strings.Contains(out1[0].Content, "偏好简短") {
		t.Fatal("plan/memory content leaked into the stable prefix")
	}
	// 尾部注入一律语义信封,并带来源声明。
	var sawPlan, sawMemory bool
	for _, m := range out2[1:] {
		if m.Role != schema.System {
			continue
		}
		switch {
		case strings.Contains(m.Content, "任务计划 v2"):
			sawPlan = true
			if !reminder.Is(m.Content) || !strings.Contains(m.Content, `source="plan"`) {
				t.Fatalf("plan must be enveloped with source, got %q", m.Content)
			}
		case strings.Contains(m.Content, "偏好简短"):
			sawMemory = true
			if !reminder.Is(m.Content) || !strings.Contains(m.Content, `source="memory"`) {
				t.Fatalf("memory must be enveloped with source, got %q", m.Content)
			}
		}
	}
	if !sawPlan || !sawMemory {
		t.Fatalf("trailing reminders missing: plan=%v memory=%v", sawPlan, sawMemory)
	}
	// L1 契约在场:信封语义教一次,全部注入通用。
	if !strings.Contains(out1[0].Content, "<system-reminder>") {
		t.Fatal("L1 must carry the injected-context contract")
	}
}

// TestEnvelopedInjections:执行记录/交互记录/fork 标注/失败记录的信封形态。
func TestEnvelopedInjections(t *testing.T) {
	tm := TrajectoryMessage([]ToolRecord{{Name: "get_x", Args: "{}", Result: "ok"}}, RecordSummary)
	if !reminder.Is(tm.Content) || !strings.Contains(tm.Content, `source="trajectory"`) ||
		!strings.Contains(tm.Content, "[执行记录]") {
		t.Fatalf("trajectory envelope wrong: %q", tm.Content)
	}

	snap := []*schema.Message{schema.UserMessage("背景")}
	ctx := WithConversationSnapshot(runctx.WithForkContext(context.Background()), snap)
	msgs := ForkMessages(ctx, schema.UserMessage("任务"))
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "caller's conversation") {
			found = true
			if !reminder.Is(m.Content) || !strings.Contains(m.Content, `source="fork-context"`) {
				t.Fatalf("fork annotation must be enveloped, got %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatal("fork annotation missing")
	}
}
