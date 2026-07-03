package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/runctx"
)

// TestContextTokensUsageCalibration 验证 token 阈值优先用供应商真实用量。
func TestContextTokensUsageCalibration(t *testing.T) {
	withUsage := schema.AssistantMessage("答", nil)
	withUsage.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 5000}}
	msgs := []*schema.Message{
		schema.UserMessage("短"), // 字符估算远小于 5000
		withUsage,
		schema.UserMessage("后续一条"),
	}
	cfg := CompactionConfig{MaxTokens: 4000}
	if !cfg.Over(msgs) {
		t.Fatal("real usage (5000) should trip the 4000 threshold despite tiny char estimate")
	}
	// 无用量:退化为字符估算,不触发
	plain := []*schema.Message{schema.UserMessage("短"), schema.AssistantMessage("答", nil)}
	if cfg.Over(plain) {
		t.Fatal("char estimate of tiny messages must not trip 4000")
	}
}

// TestSummarizeConfigurablePrompt 验证摘要内容策略可配置,归并指令
// 由框架无条件追加。
func TestSummarizeConfigurablePrompt(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("摘要", nil))
	cfg := CompactionConfig{Prompt: prompt.Value{Literal: "保留全部错误码与工单号。"}}
	if err := cfg.ResolvePrompt(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	sys := cfg.summarizePrompt()
	if !strings.Contains(sys, "错误码与工单号") {
		t.Fatalf("custom content policy missing: %q", sys)
	}
	if !strings.Contains(sys, "[已有摘要]") {
		t.Fatalf("framework merge clause must survive override: %q", sys)
	}
	if _, err := Summarize(context.Background(), m, cfg, []*schema.Message{schema.UserMessage("x")}); err != nil {
		t.Fatal(err)
	}
	// 缺省:内置内容策略 + 归并指令
	def := CompactionConfig{}
	if !strings.Contains(def.summarizePrompt(), "用户目标") || !strings.Contains(def.summarizePrompt(), "[已有摘要]") {
		t.Fatalf("default prompt malformed: %q", def.summarizePrompt())
	}
}

// TestCompactorAnchorsFirstUserMessage 验证轮内压缩的锚定:被摘要
// 覆盖段的首条用户消息原文常驻视图。
func TestCompactorAnchorsFirstUserMessage(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("压缩摘要", nil),
		schema.AssistantMessage("压缩摘要2", nil),
	)
	rewriter := Compactor(m, CompactionConfig{MaxMessages: 4, KeepRecent: 2})
	ctx := runctx.With(context.Background(), "a", "anchor-test")

	msgs := []*schema.Message{
		schema.UserMessage("最初任务:清理测试数据,生产库绝对不动"),
		schema.AssistantMessage("收到", nil),
		schema.UserMessage("继续"),
		schema.AssistantMessage("进行中", nil),
		schema.UserMessage("现在呢"),
	}
	out := rewriter(ctx, msgs)
	if !strings.Contains(out[0].Content, "压缩摘要") {
		t.Fatalf("out[0] = %q", out[0].Content)
	}
	if out[1].Role != schema.User || !strings.Contains(out[1].Content, "生产库绝对不动") {
		t.Fatalf("anchor missing, out[1] = %+v", out[1])
	}
}

// TestCompactorCacheScoped 验证缓存键含执行域:并行调用不互相抖动。
func TestCompactorCacheScoped(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("域A摘要", nil),
		schema.AssistantMessage("域B摘要", nil),
	)
	rewriter := Compactor(m, CompactionConfig{MaxMessages: 2, KeepRecent: 1})
	base := runctx.With(context.Background(), "a", "s")
	mk := func(tag string) []*schema.Message {
		return []*schema.Message{
			schema.UserMessage(tag + "-1"),
			schema.AssistantMessage(tag+"-2", nil),
			schema.UserMessage(tag + "-3"),
		}
	}
	outA := rewriter(runctx.WithScopePush(base, "comp:x#1"), mk("A"))
	outB := rewriter(runctx.WithScopePush(base, "comp:y#1"), mk("B"))
	// 两个域各自压缩、缓存互不覆盖:再次进入域 A 仍复用域 A 的摘要
	outA2 := rewriter(runctx.WithScopePush(base, "comp:x#1"), mk("A"))
	if !strings.Contains(outA[0].Content, "域A摘要") || !strings.Contains(outB[0].Content, "域B摘要") {
		t.Fatalf("scoped compaction wrong: %q %q", outA[0].Content, outB[0].Content)
	}
	if !strings.Contains(outA2[0].Content, "域A摘要") {
		t.Fatalf("scope A cache lost: %q", outA2[0].Content)
	}
	if m.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (cache hit on revisit)", m.Calls)
	}
}
