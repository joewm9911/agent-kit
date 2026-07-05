package loop

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// flakyModel 前 failures 次返回 err,之后成功。
type flakyModel struct {
	failures int32
	err      error
	calls    int32
}

func (f *flakyModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= atomic.LoadInt32(&f.failures) {
		return nil, f.err
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (f *flakyModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := f.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

func (f *flakyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

func TestRetryModelTransient(t *testing.T) {
	f := &flakyModel{failures: 2, err: errors.New("429 too many requests")}
	m := RetryModel(f, RetryConfig{MaxAttempts: 3, BaseDelay: Duration(time.Millisecond)})
	out, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "ok" {
		t.Fatalf("expect success after retries, got %v %v", out, err)
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d, want 3", f.calls)
	}
}

func TestRetryModelNonTransient(t *testing.T) {
	f := &flakyModel{failures: 10, err: errors.New("invalid api key")}
	m := RetryModel(f, RetryConfig{MaxAttempts: 3, BaseDelay: Duration(time.Millisecond)})
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err == nil {
		t.Fatal("expect error")
	}
	if f.calls != 1 {
		t.Fatalf("non-transient error should not retry, calls = %d", f.calls)
	}
}

func TestRetryModelExhausted(t *testing.T) {
	f := &flakyModel{failures: 10, err: errors.New("503 service unavailable")}
	m := RetryModel(f, RetryConfig{MaxAttempts: 2, BaseDelay: Duration(time.Millisecond)})
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err == nil {
		t.Fatal("expect error after attempts exhausted")
	}
	if f.calls != 2 {
		t.Fatalf("calls = %d, want 2", f.calls)
	}
}

func TestTimeoutToolsHang(t *testing.T) {
	hang := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "hang"},
	}, func(ctx context.Context, _ string) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "done", nil
		}
	})
	wrapped := TimeoutTools([]capability.Capability{hang}, 50*time.Millisecond)
	out, err := capability.Invoke(context.Background(), wrapped[0], "{}")
	if err != nil {
		t.Fatalf("timeout should return message, not error: %v", err)
	}
	if !strings.Contains(out, "超过") {
		t.Fatalf("got %q", out)
	}
}

func TestTimeoutToolsParentCancel(t *testing.T) {
	hang := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "hang"},
	}, func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	wrapped := TimeoutTools([]capability.Capability{hang}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := capability.Invoke(ctx, wrapped[0], "{}"); err == nil {
		t.Fatal("parent cancel should propagate as error")
	}
}

func TestTimeoutToolsFastPath(t *testing.T) {
	fast := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "fast"},
	}, func(ctx context.Context, _ string) (string, error) { return "quick", nil })
	wrapped := TimeoutTools([]capability.Capability{fast}, time.Minute)
	out, err := capability.Invoke(context.Background(), wrapped[0], "{}")
	if err != nil || out != "quick" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestApprovalGateCtxMode(t *testing.T) {
	mut := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "t", Name: "write"},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) { return "executed", nil })

	gated := GateApprovalCtx([]capability.Capability{mut})

	// ctx 无模式:缺省 interactive,无交互通道 → 拦截
	out, err := capability.Invoke(context.Background(), gated[0], "{}")
	if err != nil || !strings.Contains(out, "无交互通道") {
		t.Fatalf("got %q, %v", out, err)
	}
	// ctx 装入 auto:直接放行
	ctx := WithApprovalMode(context.Background(), ApprovalAuto)
	if out, _ = capability.Invoke(ctx, gated[0], "{}"); out != "executed" {
		t.Fatalf("auto mode should pass, got %q", out)
	}
	// ctx 装入 deny:拒绝
	ctx = WithApprovalMode(context.Background(), ApprovalDeny)
	if out, _ = capability.Invoke(ctx, gated[0], "{}"); !strings.Contains(out, "只读模式") {
		t.Fatalf("deny mode should block, got %q", out)
	}
	// ctx 装入 interactive + 批准通道:执行
	ctx = WithApprovalMode(runctx.WithInteractor(context.Background(), stubInteractor{approve: true}), ApprovalInteractive)
	if out, _ = capability.Invoke(ctx, gated[0], "{}"); out != "executed" {
		t.Fatalf("approved op should execute, got %q", out)
	}
}
