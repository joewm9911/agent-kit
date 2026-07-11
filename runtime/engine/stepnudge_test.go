package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
)

// 步数收口引导:进入最后一轮工具机会时注入、只注入一次、小上限不注入。
func TestStepNudgeModifier(t *testing.T) {
	ctx := runctx.WithTurnState(context.Background())
	nudge := stepNudgeModifier(4)
	base := []*schema.Message{schema.UserMessage("任务")}
	var injectedAt []int
	for i := 1; i <= 5; i++ {
		out := nudge(ctx, base)
		for _, m := range out {
			if strings.Contains(m.Content, "[步数提醒]") {
				injectedAt = append(injectedAt, i)
			}
		}
	}
	if len(injectedAt) != 1 || injectedAt[0] != 4 {
		t.Fatalf("应只在第 4 轮注入一次,得 %v", injectedAt)
	}

	// 上限太小不注入
	ctx2 := runctx.WithTurnState(context.Background())
	small := stepNudgeModifier(2)
	for i := 0; i < 3; i++ {
		for _, m := range small(ctx2, base) {
			if strings.Contains(m.Content, "[步数提醒]") {
				t.Fatalf("rounds<3 不应注入")
			}
		}
	}

	// 执行域隔离:子域计数独立
	ctx3 := runctx.WithTurnState(context.Background())
	n3 := stepNudgeModifier(3)
	n3(ctx3, base)
	sub := runctx.WithScopePush(ctx3, "comp:x#1")
	for i := 0; i < 2; i++ {
		for _, m := range n3(sub, base) {
			if strings.Contains(m.Content, "[步数提醒]") {
				t.Fatalf("子域第 %d 轮不该注入(计数应独立)", i+1)
			}
		}
	}
}
