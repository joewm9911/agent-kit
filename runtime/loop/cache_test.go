package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
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

// TestModifierUserInEnv 验证终端用户身份并入环境信息块:有用户则注入、
// 无用户不注入(不留空值)、业务自定义 Env 的同名键不被覆盖。
func TestModifierUserInEnv(t *testing.T) {
	mod := PromptLayers{}.Modifier()
	in := []*schema.Message{schema.UserMessage("hi")}

	// 有用户:环境信息块出现「用户: u42」
	ctx := runctx.WithUser(context.Background(), "u42")
	head := mod(ctx, in)[0].Content
	if !strings.Contains(head, "# 环境信息") || !strings.Contains(head, "用户: u42") {
		t.Fatalf("user id should be injected into env block, got:\n%s", head)
	}

	// 无用户:不注入「用户」键
	if h := mod(context.Background(), in)[0].Content; strings.Contains(h, "用户:") {
		t.Fatalf("no user must not inject empty 用户 key, got:\n%s", h)
	}

	// 业务自定义 Env 已给「用户」:不覆盖
	custom := PromptLayers{Env: func(context.Context) map[string]string {
		return map[string]string{"用户": "biz-alias"}
	}}.Modifier()
	if h := custom(runctx.WithUser(context.Background(), "u42"), in)[0].Content; !strings.Contains(h, "用户: biz-alias") {
		t.Fatalf("business-provided 用户 must not be clobbered, got:\n%s", h)
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

// TestModifierFocusOrder 验证 Focus 层:本轮用户问题重述占据消息最尾
// (记忆、计划之后——注意力排序 = 当前问题 > 计划 > 记忆);超长截断;
// 未开启/无输入时不注入。
func TestModifierFocusOrder(t *testing.T) {
	layers := PromptLayers{
		Memories: func(context.Context) []string { return []string{"记忆片段"} },
		Plan:     func(context.Context) string { return "# 当前任务计划\n☐ 事项" },
		Focus:    true,
	}
	mod := layers.Modifier()
	ctx := runctx.WithInput(context.Background(), "我们在卖哪些商品?")
	out := mod(ctx, []*schema.Message{schema.UserMessage("我们在卖哪些商品?")})

	n := len(out)
	if !strings.Contains(out[n-1].Content, "本轮用户问题") || !strings.Contains(out[n-1].Content, "我们在卖哪些商品") {
		t.Fatalf("focus restatement must be the last message, got %q", out[n-1].Content)
	}
	if !strings.Contains(out[n-2].Content, "任务计划") || !strings.Contains(out[n-3].Content, "记忆片段") {
		t.Fatalf("tail order must be memories < plan < focus, got %q / %q", out[n-3].Content, out[n-2].Content)
	}

	// 超长输入截断,提示原文在上方
	long := strings.Repeat("问", 400)
	out = mod(runctx.WithInput(context.Background(), long), []*schema.Message{schema.UserMessage(long)})
	tail := out[len(out)-1].Content
	if strings.Contains(tail, long) || !strings.Contains(tail, "截断") {
		t.Fatalf("long input must be truncated in restatement, len=%d", len(tail))
	}

	// Focus 关闭 / 无输入:不注入
	off := PromptLayers{Plan: layers.Plan}
	out = off.Modifier()(ctx, []*schema.Message{schema.UserMessage("q")})
	if strings.Contains(out[len(out)-1].Content, "本轮用户问题") {
		t.Fatal("focus must be opt-in")
	}
	out = mod(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if strings.Contains(out[len(out)-1].Content, "本轮用户问题") {
		t.Fatal("no input, no restatement")
	}
}
