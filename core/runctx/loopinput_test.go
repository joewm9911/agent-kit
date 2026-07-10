package runctx

import (
	"context"
	"testing"
)

// TestLoopInputSetOnce 验证 {$user_input} 语义:顶层设定一次、嵌套不被覆盖;
// 未设时回落到作用域 Input。
func TestLoopInputSetOnce(t *testing.T) {
	base := context.Background()

	// 未注入:LoopInput 回落到 Input(顶层与作用域相等,向后兼容)
	c0 := WithInput(base, "原始问题")
	if got := LoopInput(c0); got != "原始问题" {
		t.Fatalf("fallback to Input failed: %q", got)
	}

	// 顶层设定 loop input
	c1 := WithLoopInput(base, "顶层任务")
	if got := LoopInput(c1); got != "顶层任务" {
		t.Fatalf("LoopInput = %q, want 顶层任务", got)
	}

	// 嵌套:作用域 Input 被重设,但 LoopInput 恒定(set-once)
	c2 := WithInput(c1, "子任务华东")
	c2 = WithLoopInput(c2, "试图覆盖") // 应被忽略
	if got := Input(c2); got != "子任务华东" {
		t.Fatalf("scoped Input = %q, want 子任务华东", got)
	}
	if got := LoopInput(c2); got != "顶层任务" {
		t.Fatalf("LoopInput must stay set-once, got %q", got)
	}
}
