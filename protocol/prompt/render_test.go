package prompt

import "testing"

// TestRenderSinglePass(L3 回归):值里携带的 {占位} 字面量不得被二次展开
// (旧实现逐参数 ReplaceAll,展开与否取决于 map 随机迭代序——不确定性)。
func TestRenderSinglePass(t *testing.T) {
	tpl := &Template{Text: "A={a} B={b}"}
	got := tpl.Render(map[string]string{"a": "{b}", "b": "世界"})
	if got != "A={b} B=世界" {
		t.Fatalf("value-borne placeholder must not re-expand: %q", got)
	}
	if tpl2 := (&Template{Text: "{x} {y}"}); tpl2.Render(map[string]string{"x": "1"}) != "1 {y}" {
		t.Fatal("unknown placeholder must stay literal")
	}
}
