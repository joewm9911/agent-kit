package loop

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

func mutCap(name string, executed *int32) capability.Capability {
	return capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "im", Name: name},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) {
		atomic.AddInt32(executed, 1)
		return "sent", nil
	})
}

func TestApprovalPolicyRules(t *testing.T) {
	var executed int32
	gated := GateApprovalCtx([]capability.Capability{mutCap("send_message", &executed)})

	st, err := NewApprovalState(ApprovalInteractive, ApprovalPolicy{
		Rules: []ApprovalRule{
			{Ref: "cap://tool/im/send_message", Args: map[string]string{"to": "team-*"}, Action: "allow"},
			{Ref: "cap://tool/im/send_message", Args: map[string]string{"to": "boss"}, Action: "deny"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithApprovalState(runctx.With(context.Background(), "a", "s1"), st)

	// 命中 allow 规则:免批直接执行(无交互通道也能过)
	out, err := capability.Invoke(ctx, gated[0], `{"to":"team-dev","text":"hi"}`)
	if err != nil || out != "sent" {
		t.Fatalf("allow rule failed: %q %v", out, err)
	}
	if executed != 1 {
		t.Fatalf("executed = %d", executed)
	}

	// 命中 deny 规则:直接拒绝
	out, _ = capability.Invoke(ctx, gated[0], `{"to":"boss","text":"hi"}`)
	if !strings.Contains(out, "deny 规则") || executed != 1 {
		t.Fatalf("deny rule failed: %q executed=%d", out, executed)
	}

	// 无规则命中:回落 ask,无交互通道 → 拦截
	out, _ = capability.Invoke(ctx, gated[0], `{"to":"external","text":"hi"}`)
	if !strings.Contains(out, "无交互通道") || executed != 1 {
		t.Fatalf("fallback ask failed: %q executed=%d", out, executed)
	}
}

func TestApprovalPolicyBadAction(t *testing.T) {
	_, err := NewApprovalState(ApprovalInteractive, ApprovalPolicy{
		Rules: []ApprovalRule{{Ref: "*", Action: "yolo"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad action") {
		t.Fatalf("expect action validation error, got %v", err)
	}
}

// decisionStub 按脚本回答审批决定。
type decisionStub struct {
	decision Decision
	asked    int32
}

func (d *decisionStub) Ask(context.Context, string) (string, error) { return "", nil }
func (d *decisionStub) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return false, nil
}
func (d *decisionStub) ApproveDecision(context.Context, runctx.ApprovalRequest) (Decision, error) {
	atomic.AddInt32(&d.asked, 1)
	return d.decision, nil
}

func TestApprovalDecisionMemory(t *testing.T) {
	var executed int32
	gated := GateApprovalCtx([]capability.Capability{mutCap("send_message", &executed)})

	st, err := NewApprovalState(ApprovalInteractive, ApprovalPolicy{Remember: true})
	if err != nil {
		t.Fatal(err)
	}
	it := &decisionStub{decision: DecisionAlwaysAllow}
	ctx := WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "s1"), it), st)

	// 第一次:询问,回答"总是允许" → 执行并记住
	if out, _ := capability.Invoke(ctx, gated[0], `{"to":"x"}`); out != "sent" {
		t.Fatalf("got %q", out)
	}
	// 第二次:不再询问,直接放行
	if out, _ := capability.Invoke(ctx, gated[0], `{"to":"y"}`); out != "sent" {
		t.Fatalf("got %q", out)
	}
	if it.asked != 1 {
		t.Fatalf("interactor asked %d times, want 1 (memory should skip)", it.asked)
	}
	if executed != 2 {
		t.Fatalf("executed = %d", executed)
	}

	// 记忆按会话隔离:另一个会话重新询问
	ctx2 := WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "s2"), it), st)
	if out, _ := capability.Invoke(ctx2, gated[0], `{"to":"z"}`); out != "sent" {
		t.Fatalf("got %q", out)
	}
	if it.asked != 2 {
		t.Fatalf("new session should re-ask, asked = %d", it.asked)
	}
}

func TestApprovalAlwaysDenyMemory(t *testing.T) {
	var executed int32
	gated := GateApprovalCtx([]capability.Capability{mutCap("send_message", &executed)})

	st, _ := NewApprovalState(ApprovalInteractive, ApprovalPolicy{Remember: true})
	it := &decisionStub{decision: DecisionAlwaysDeny}
	ctx := WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "s1"), it), st)

	out, _ := capability.Invoke(ctx, gated[0], `{}`)
	if !strings.Contains(out, "拒绝") || executed != 0 {
		t.Fatalf("got %q executed=%d", out, executed)
	}
	out, _ = capability.Invoke(ctx, gated[0], `{}`)
	if !strings.Contains(out, "已在本会话拒绝") || executed != 0 || it.asked != 1 {
		t.Fatalf("got %q executed=%d asked=%d", out, executed, it.asked)
	}
}
