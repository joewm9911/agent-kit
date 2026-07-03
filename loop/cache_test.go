package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/runctx"
)

// TestCompactorStablePrefix 验证压缩是低频一次性事件:压缩之后、
// 再次超阈值之前,视图前缀保持逐字节稳定(prompt cache 可命中),
// 且不再触发摘要模型调用。
func TestCompactorStablePrefix(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("摘要一", nil),
		schema.AssistantMessage("摘要二", nil),
	)
	rewriter := Compactor(m, CompactionConfig{MaxMessages: 6, KeepRecent: 2})
	ctx := runctx.With(context.Background(), "a", "s1")

	msgs := []*schema.Message{
		schema.UserMessage("q1"), schema.AssistantMessage("a1", nil),
		schema.UserMessage("q2"), schema.AssistantMessage("a2", nil),
		schema.UserMessage("q3"), schema.AssistantMessage("a3", nil),
		schema.UserMessage("q4"),
	}
	v1 := rewriter(ctx, msgs)
	if m.Calls != 1 || !strings.Contains(v1[0].Content, "摘要一") {
		t.Fatalf("first compaction: calls=%d head=%q", m.Calls, v1[0].Content)
	}

	// 追加一条消息(仍在阈值内):复用缓存,不再调用摘要,前缀不变
	msgs2 := append(append([]*schema.Message{}, msgs...), schema.AssistantMessage("a4", nil))
	v2 := rewriter(ctx, msgs2)
	if m.Calls != 1 {
		t.Fatalf("stable window should not re-summarize, calls = %d", m.Calls)
	}
	if v2[0].Content != v1[0].Content {
		t.Fatal("prefix changed between compactions (cache-busting)")
	}

	// 继续膨胀直到视图再次超阈值:增量归并,一次新摘要
	msgs3 := append(append([]*schema.Message{}, msgs2...),
		schema.UserMessage("q5"), schema.AssistantMessage("a5", nil),
		schema.UserMessage("q6"), schema.AssistantMessage("a6", nil),
	)
	v3 := rewriter(ctx, msgs3)
	if m.Calls != 2 {
		t.Fatalf("expect one incremental re-summarize, calls = %d", m.Calls)
	}
	if !strings.Contains(v3[0].Content, "摘要二") {
		t.Fatalf("head = %q", v3[0].Content)
	}
}

// TestModifierDeterministic 验证头部 system prompt 逐次调用稳定
// (环境信息排序、记忆不进头部)。
func TestModifierDeterministic(t *testing.T) {
	calls := 0
	layers := PromptLayers{
		Persona: "你是助手",
		Env: func(context.Context) map[string]string {
			return map[string]string{"b": "2", "a": "1", "c": "3"}
		},
		Memories: func(context.Context) []string {
			calls++
			return []string{"记忆片段"}
		},
	}
	mod := layers.Modifier()
	in := []*schema.Message{schema.UserMessage("hi")}

	out1 := mod(context.Background(), in)
	out2 := mod(context.Background(), in)
	if out1[0].Content != out2[0].Content {
		t.Fatal("head system prompt should be identical across calls")
	}
	if strings.Contains(out1[0].Content, "记忆片段") {
		t.Fatal("memories must not pollute the stable head prompt")
	}
	last := out1[len(out1)-1]
	if !strings.Contains(last.Content, "记忆片段") {
		t.Fatalf("memories should be appended at tail, got %q", last.Content)
	}
}

// TestEstimateCJK 验证按字符类型的 token 估算。
func TestEstimateCJK(t *testing.T) {
	en := []*schema.Message{schema.UserMessage(strings.Repeat("word", 100))} // 400 ASCII
	if got := estimate(en); got < 90 || got > 110 {
		t.Fatalf("english estimate = %d, want ~100", got)
	}
	zh := []*schema.Message{schema.UserMessage(strings.Repeat("中", 100))} // 100 CJK
	if got := estimate(zh); got < 65 || got > 85 {
		t.Fatalf("cjk estimate = %d, want ~75", got)
	}
}
