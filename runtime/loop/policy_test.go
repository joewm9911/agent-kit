package loop

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"fmt"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"sync"
	"time"
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
	}, nil, 0)
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
	}, nil, 0)
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

	st, err := NewApprovalState(ApprovalInteractive, ApprovalPolicy{Remember: true}, nil, 0)
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

	st, _ := NewApprovalState(ApprovalInteractive, ApprovalPolicy{Remember: true}, nil, 0)
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

// slowDecisionStub 答复前有延迟,模拟真人:并发弹窗竞态的复现件。
type slowDecisionStub struct {
	decision Decision
	asked    int32
	delay    time.Duration
}

func (d *slowDecisionStub) Ask(context.Context, string) (string, error) { return "", nil }
func (d *slowDecisionStub) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return false, nil
}
func (d *slowDecisionStub) ApproveDecision(context.Context, runctx.ApprovalRequest) (Decision, error) {
	atomic.AddInt32(&d.asked, 1)
	time.Sleep(d.delay)
	return d.decision, nil
}

// TestApprovalConcurrentBurstAsksOnce:模型并行发起一批同能力调用时,
// 全部 goroutine 会在首个弹窗被回答前越过记忆检查——锁内重查保证
// 用户第一次"总是允许"覆盖已排队的其余调用,总共只弹一次窗。
// (实测回归:11 笔并行 ship-order 连弹 11 窗,按键错位致部分误拒。)
func TestApprovalConcurrentBurstAsksOnce(t *testing.T) {
	var executed int32
	gated := GateApprovalCtx([]capability.Capability{mutCap("ship_order", &executed)})
	st, err := NewApprovalState(ApprovalInteractive, ApprovalPolicy{Remember: true}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	it := &slowDecisionStub{decision: DecisionAlwaysAllow, delay: 50 * time.Millisecond}
	ctx := WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "burst"), it), st)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := capability.Invoke(ctx, gated[0], fmt.Sprintf(`{"id":"O-%d"}`, i))
			if err != nil || out != "sent" {
				t.Errorf("call %d: %q %v", i, out, err)
			}
		}(i)
	}
	wg.Wait()
	if it.asked != 1 {
		t.Fatalf("interactor asked %d times, want exactly 1(锁内重查失效)", it.asked)
	}
	if executed != n {
		t.Fatalf("executed = %d, want %d", executed, n)
	}
}

// TestDeniedCallsCheck:被拒调用记入轮状态袋,收口检查弹回一次要求
// 如实区分;无拒绝/无轮语义/已催过均不弹。
func TestDeniedCallsCheck(t *testing.T) {
	var executed int32
	gated := GateApprovalCtx([]capability.Capability{mutCap("refund", &executed)})
	st, _ := NewApprovalState(ApprovalInteractive, ApprovalPolicy{}, nil, 0)
	it := &decisionStub{decision: DecisionDeny}
	ctx := runctx.WithTurnState(
		WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "dn"), it), st))

	if msg := DeniedCallsCheck(ctx); msg != "" {
		t.Fatalf("no denials yet, got %q", msg)
	}
	out, _ := capability.Invoke(ctx, gated[0], `{"id":"O-1"}`)
	if !strings.Contains(out, "用户拒绝") || executed != 0 {
		t.Fatalf("expect denial result, got %q executed=%d", out, executed)
	}
	msg := DeniedCallsCheck(ctx)
	if !strings.Contains(msg, "1 个调用被用户拒绝") || !strings.Contains(msg, "refund") {
		t.Fatalf("expect denial nag, got %q", msg)
	}
	if again := DeniedCallsCheck(ctx); again != "" {
		t.Fatalf("nag at most once per turn, got %q", again)
	}
	// 无轮语义:透传
	bare := WithApprovalState(runctx.WithInteractor(runctx.With(context.Background(), "a", "dn2"), it), st)
	_, _ = capability.Invoke(bare, gated[0], `{"id":"O-2"}`)
	if msg := DeniedCallsCheck(bare); msg != "" {
		t.Fatalf("no turn state must not nag, got %q", msg)
	}
}
