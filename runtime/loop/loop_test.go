package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func TestBudgetHardStop(t *testing.T) {
	m := BudgetModel(testmodel.New())
	gate := NewBudgetGate(BudgetConfig{MaxModelCalls: 2}, nil, 0)
	ctx := WithBudget(runctx.With(context.Background(), "a", "s1"), gate)
	for i := 0; i < 2; i++ {
		if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("q")}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("q")})
	var exhausted *ErrBudgetExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("expect ErrBudgetExhausted, got %v", err)
	}
	if calls, _ := gate.Spend(ctx); calls != 2 {
		t.Fatalf("spend = %d", calls)
	}
	// 预算按会话隔离:另一个会话不受影响
	ctx2 := WithBudget(runctx.With(context.Background(), "a", "s2"), gate)
	if _, err := m.Generate(ctx2, []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("other session should not be limited: %v", err)
	}
	// 未装门闸的 ctx:透传不设限
	bare := runctx.With(context.Background(), "a", "s1")
	if _, err := m.Generate(bare, []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("no gate should mean no limit: %v", err)
	}
}

func TestStructuredEnforceWithRetry(t *testing.T) {
	e, err := NewStructuredEnforcer(StructuredConfig{
		Schema: `{"type":"object","required":["city"],"properties":{"city":{"type":"string"}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 第一次输出不合规,模型修复后合规。
	m := testmodel.New(schema.AssistantMessage(`{"city":"北京"}`, nil))
	out, err := e.Enforce(context.Background(), m, "这不是 JSON")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "北京") {
		t.Fatalf("got %q", out)
	}
}

func TestCompactorKeepsToolPairing(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("摘要", nil))
	rewriter := Compactor(m, CompactionConfig{MaxMessages: 4, KeepRecent: 2})

	msgs := []*schema.Message{
		schema.UserMessage("q1"),
		schema.AssistantMessage("a1", nil),
		testmodel.ToolCallMsg("t", "{}"),
		schema.ToolMessage("result", "call-t"),
		schema.UserMessage("q2"),
		schema.AssistantMessage("a2", nil),
	}
	out := rewriter(context.Background(), msgs)
	if out[0].Role != schema.System || !strings.Contains(out[0].Content, "摘要") {
		t.Fatalf("first message should be summary, got %+v", out[0])
	}
	// 切割点不能落在 tool 响应上
	if out[1].Role == schema.Tool {
		t.Fatal("compaction split tool-call pairing")
	}
}

func TestApprovalGate(t *testing.T) {
	mut := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "t", Name: "write"},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) { return "executed", nil })
	ro := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "t", Name: "read"},
		Risk: capability.RiskReadonly, // 未声明现按 mutating 保守对待,只读需显式
	}, func(ctx context.Context, in string) (string, error) { return "read-ok", nil })

	gatedCaps := GateApproval([]capability.Capability{mut, ro}, ApprovalInteractive)

	// 无交互通道:mutating 被拦截,readonly 放行
	out, err := capability.Invoke(context.Background(), gatedCaps[0], "{}")
	if err != nil || !strings.Contains(out, "无交互通道") {
		t.Fatalf("got %q, %v", out, err)
	}
	out, _ = capability.Invoke(context.Background(), gatedCaps[1], "{}")
	if out != "read-ok" {
		t.Fatalf("readonly should pass, got %q", out)
	}

	// 有交互通道:批准 → 执行;拒绝 → 拦截
	ctx := runctx.WithInteractor(context.Background(), stubInteractor{approve: true})
	if out, _ = capability.Invoke(ctx, gatedCaps[0], "{}"); out != "executed" {
		t.Fatalf("approved op should execute, got %q", out)
	}
	ctx = runctx.WithInteractor(context.Background(), stubInteractor{approve: false})
	if out, _ = capability.Invoke(ctx, gatedCaps[0], "{}"); !strings.Contains(out, "拒绝") {
		t.Fatalf("denied op should be blocked, got %q", out)
	}
}

type stubInteractor struct{ approve bool }

func (s stubInteractor) Ask(context.Context, string) (string, error) { return "answer", nil }
func (s stubInteractor) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return s.approve, nil
}

func TestTruncateResults(t *testing.T) {
	big := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "big"},
	}, func(ctx context.Context, _ string) (string, error) {
		return strings.Repeat("长", 9000), nil
	})
	wrapped := TruncateResults([]capability.Capability{big}, 0) // 0 = 默认 8000
	out, err := capability.Invoke(context.Background(), wrapped[0], "{}")
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(out)) > 8200 || !strings.Contains(out, "已截断") {
		t.Fatalf("truncation failed: len=%d", len([]rune(out)))
	}
	// -1 关闭截断
	off := TruncateResults([]capability.Capability{big}, -1)
	out, _ = capability.Invoke(context.Background(), off[0], "{}")
	if len([]rune(out)) != 9000 {
		t.Fatalf("disable failed: len=%d", len([]rune(out)))
	}
}
