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
// 没有守卫时 Run 会直接返回第一段文本("做完了。"),不会消费后续脚本。
func TestTodoFinishReconcileWiring(t *testing.T) {
	m := testmodel.New(
		testmodel.ToolCallMsg("todo_write",
			`{"todos":[{"content":"查A","status":"in_progress"},{"content":"查B","status":"pending"}]}`),
		schema.AssistantMessage("做完了。", nil), // 计划仍有 2 项未收口 → 弹回
		testmodel.ToolCallMsg("todo_write",
			`{"todos":[{"content":"查A","status":"completed"},{"content":"查B","status":"completed"}]}`),
		schema.AssistantMessage("最终回答", nil), // 已收口 → 放行
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
