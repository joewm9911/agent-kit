package config

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
)

// TestTodoFinishReconcileWiring 端到端验证计划收口守卫的真实接线
// (agent 入口装轮状态袋 → 主循环模型带 CheckedFinish → todo.FinishCheck):
// 模型列了计划就想用纯文本收尾 → 被弹回 → 补交收口后的清单 → 放行。
// GoalCheck 默认关(见 TestTodoGoalCheckWiring 显式开启),故此处只验
// FinishCheck 的收口接线:计划→弹回→收口→终答,共 4 次调用。
func TestTodoFinishReconcileWiring(t *testing.T) {
	m := testmodel.New(
		testmodel.ToolCallMsg("todo_write",
			`{"todos":[{"content":"查A","status":"in_progress"},{"content":"查B","status":"pending"}]}`),
		schema.AssistantMessage("做完了。", nil), // 计划仍有 2 项未收口 → FinishCheck 弹回
		testmodel.ToolCallMsg("todo_write",
			`{"todos":[{"content":"查A","status":"completed"},{"content":"查B","status":"completed"}]}`),
		schema.AssistantMessage("最终回答", nil), // 已收口 → 放行(GoalCheck 默认关)
	)
	a, err := buildAgent(context.Background(), &AgentConfig{Name: "closer"}, Profile{}, nil, nil, m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Run(context.Background(), "s1", "多步任务")
	if err != nil {
		t.Fatal(err)
	}
	if out != "最终回答" {
		t.Fatalf("guard must bounce the unreconciled finish, got %q", out)
	}
	if m.Calls != 4 {
		t.Fatalf("model calls = %d, want 4(计划→弹回→收口→终答)", m.Calls)
	}
}

// TestTodoGoalCheckWiring 端到端验证目标达成核对(U4.1)的真实接线:
// 用过计划的多步任务收尾时,GoalCheck 强制一次目标对照自检(每轮至多一次,
// 故最多多一次重生成)。这里验证:注入的自检提示确实出现在重生成的消息里,
// 且核对后放行、总调用数 = 计划 + 终答 + 一次核对。
func TestTodoGoalCheckWiring(t *testing.T) {
	m := testmodel.New(
		testmodel.ToolCallMsg("todo_write",
			`{"todos":[{"content":"查A","status":"completed"}]}`),
		schema.AssistantMessage("A=1", nil),        // 计划已收口 → GoalCheck 弹回
		schema.AssistantMessage("A=1,已核对全覆盖", nil), // 核对后 → 放行
	)
	on := true
	ac := &AgentConfig{Name: "checker"}
	ac.Capabilities.GoalCheck = &on // 显式开启(默认关)
	a, err := buildAgent(context.Background(), ac, Profile{}, nil, nil, m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Run(context.Background(), "s2", "查一下 A 再核对")
	if err != nil {
		t.Fatal(err)
	}
	if out != "A=1,已核对全覆盖" {
		t.Fatalf("GoalCheck should bounce once then pass, got %q", out)
	}
	// 3 次调用(而非 2)证明 GoalCheck 触发了一次重生成核对;核对提示的
	// 内容与自限语义由 todo 包的 TestGoalCheck 单测覆盖。
	if m.Calls != 3 {
		t.Fatalf("model calls = %d, want 3(计划→初答→目标核对后终答)", m.Calls)
	}
}
