package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
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

// TestClearOldToolResults 验证旧轮工具结果清理:保护窗外超长 tool 消息
// 被替换为占位;窗内/短结果/已消化/非 tool 一律不动;幂等;copy-on-write
// 不改原切片。
func TestClearOldToolResults(t *testing.T) {
	long := strings.Repeat("库存流水;", 300) // 1500 rune
	msgs := []*schema.Message{
		schema.UserMessage("查库存"),
		schema.ToolMessage(long, "c1"),  // 窗外+超长 → 清
		schema.ToolMessage("短结果", "c2"), // 窗外但短 → 留
		schema.ToolMessage("[结果已消化:原始 5230 字符;全文已存为 r1]"+long, "c3"), // 已消化 → 留(指针不可丢)
		schema.AssistantMessage(long, nil), // 非 tool → 留
		schema.UserMessage("继续"),
		schema.ToolMessage(long, "c4"), // 窗内 → 留
	}
	orig1 := msgs[1].Content

	out := clearOldToolResults(msgs, 800, 2)
	if !strings.HasPrefix(out[1].Content, toolClearedPrefix) || !strings.Contains(out[1].Content, "1500") {
		t.Fatalf("old long tool result must be cleared, got %.60q", out[1].Content)
	}
	if out[2].Content != "短结果" || !strings.Contains(out[3].Content, "已存为 r1") {
		t.Fatal("short and digested results must be kept")
	}
	if out[4].Content != long || out[6].Content != long {
		t.Fatal("assistant and in-window tool messages must be untouched")
	}
	if msgs[1].Content != orig1 {
		t.Fatal("copy-on-write violated: original slice mutated")
	}
	// 幂等:再清一遍无变化
	again := clearOldToolResults(out, 800, 2)
	for i := range again {
		if again[i].Content != out[i].Content {
			t.Fatalf("not idempotent at %d", i)
		}
	}
	// 关闭/窗覆盖全部:原样返回
	if got := clearOldToolResults(msgs, 0, 2); &got[1] != &msgs[1] && got[1].Content != orig1 {
		t.Fatal("over=0 must be no-op")
	}
	if got := clearOldToolResults(msgs, 800, len(msgs)); got[1].Content != orig1 {
		t.Fatal("keep >= len must be no-op")
	}
}

// TestCompactorClearOnlyMode 只配 tool_clear(不配压缩阈值)时 Compactor
// 亦启用,清理生效且不触发摘要。
func TestCompactorClearOnlyMode(t *testing.T) {
	long := strings.Repeat("数据;", 500)
	cfg := CompactionConfig{ToolClearOver: 600, ToolClearKeep: 2}
	if !cfg.Enabled() {
		t.Fatal("clear-only config must enable the rewriter")
	}
	m := testmodel.New() // 不应被调用
	rw := Compactor(m, cfg)
	msgs := []*schema.Message{
		schema.UserMessage("q"),
		schema.ToolMessage(long, "c1"),
		schema.UserMessage("q2"),
		schema.AssistantMessage("a", nil),
	}
	ctx := runctx.With(context.Background(), "a", "s")
	out := rw(ctx, msgs)
	if !strings.HasPrefix(out[1].Content, toolClearedPrefix) {
		t.Fatalf("clear must apply, got %.50q", out[1].Content)
	}
	if m.Calls != 0 {
		t.Fatalf("no summarize call expected, got %d", m.Calls)
	}
}
