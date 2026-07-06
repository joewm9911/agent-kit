package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

// dedupCap 造一个记录执行次数的能力;out 为 nil 时回显固定结果。
func dedupCap(execs *int, out func(n int) string) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "probe"},
	}, func(_ context.Context, args string) (string, error) {
		*execs++
		if out != nil {
			return out(*execs), nil
		}
		return "result", nil
	})
}

func dedupInvoke(t *testing.T, ctx context.Context, c capability.Capability, args string) string {
	t.Helper()
	s, err := capability.Invoke(ctx, c, args)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDedupCalls 验证断路器主线:第 2 次同参同结果附加提醒,第 3 次起
// 拦截回放不执行;参数变化重置。
func TestDedupCalls(t *testing.T) {
	execs := 0
	c := DedupCalls([]capability.Capability{dedupCap(&execs, nil)})[0]
	ctx := runctx.WithTurnState(runctx.With(context.Background(), "a", "s"))

	if out := dedupInvoke(t, ctx, c, `{"x":1}`); strings.Contains(out, "重复调用") {
		t.Fatalf("first call must be clean, got %q", out)
	}
	out := dedupInvoke(t, ctx, c, `{"x":1}`)
	if !strings.Contains(out, "[重复调用]") || execs != 2 {
		t.Fatalf("second identical call executes with warning, got %q execs=%d", out, execs)
	}
	out = dedupInvoke(t, ctx, c, `{"x":1}`)
	if !strings.Contains(out, "已拦截") || !strings.Contains(out, "result") || execs != 2 {
		t.Fatalf("third call must be blocked with replay, got %q execs=%d", out, execs)
	}
	// 再犯继续拦
	_ = dedupInvoke(t, ctx, c, `{"x":1}`)
	if execs != 2 {
		t.Fatalf("still blocked, execs=%d", execs)
	}
	// 换参数:重置,正常执行
	if out := dedupInvoke(t, ctx, c, `{"x":2}`); strings.Contains(out, "重复调用") || execs != 3 {
		t.Fatalf("changed args must reset, got %q execs=%d", out, execs)
	}
}

// TestDedupPollingUnaffected 结果每次变化(轮询类):永不计为重复。
func TestDedupPollingUnaffected(t *testing.T) {
	execs := 0
	c := DedupCalls([]capability.Capability{dedupCap(&execs, func(n int) string {
		return fmt.Sprintf("state-%d", n)
	})})[0]
	ctx := runctx.WithTurnState(runctx.With(context.Background(), "a", "poll"))
	for i := 0; i < 5; i++ {
		if out := dedupInvoke(t, ctx, c, `{}`); strings.Contains(out, "重复调用") {
			t.Fatalf("changing results must not trip the breaker, got %q", out)
		}
	}
	if execs != 5 {
		t.Fatalf("all polls execute, execs=%d", execs)
	}
}

// TestDedupScopedAndTurnBound 无轮语义不介入;不同执行域各自计数。
func TestDedupScopedAndTurnBound(t *testing.T) {
	execs := 0
	c := DedupCalls([]capability.Capability{dedupCap(&execs, nil)})[0]

	// 无 TurnState:透传
	bare := runctx.With(context.Background(), "a", "s")
	for i := 0; i < 3; i++ {
		if out := dedupInvoke(t, bare, c, `{}`); strings.Contains(out, "重复调用") {
			t.Fatalf("no turn state must pass through, got %q", out)
		}
	}
	if execs != 3 {
		t.Fatalf("execs=%d", execs)
	}

	// 同轮不同执行域:互不影响
	execs = 0
	ctx := runctx.WithTurnState(runctx.With(context.Background(), "a", "s"))
	sub := runctx.WithScopePush(ctx, "skill:x")
	_ = dedupInvoke(t, ctx, c, `{}`)
	_ = dedupInvoke(t, ctx, c, `{}`) // 主域第 2 次:提醒
	if out := dedupInvoke(t, sub, c, `{}`); strings.Contains(out, "重复调用") {
		t.Fatalf("sub scope counts independently, got %q", out)
	}
	// 新一轮(新状态袋):计数清零
	turn2 := runctx.WithTurnState(runctx.With(context.Background(), "a", "s"))
	if out := dedupInvoke(t, turn2, c, `{}`); strings.Contains(out, "重复调用") {
		t.Fatalf("new turn resets counts, got %q", out)
	}
}

// TestRepeatBreak 验证模型层终止器:热点调用先弹回纠正;纠正后仍发起
// 同一调用 → 强制收束为引用真实结果的最终文本;无热点/不匹配透传。
func TestRepeatBreak(t *testing.T) {
	execs := 0
	c := DedupCalls([]capability.Capability{dedupCap(&execs, nil)})[0]
	ctx := runctx.WithTurnState(runctx.With(context.Background(), "a", "rb"))
	// 打满热点阈值:2 次执行 + 2 次拦截
	for i := 0; i < dedupHotAfter; i++ {
		_ = dedupInvoke(t, ctx, c, `{"x":1}`)
	}
	if hotEntry(ctx) == nil {
		t.Fatal("hot entry must be set after threshold")
	}

	call := schema.AssistantMessage("", []schema.ToolCall{{ID: "c", Type: "function",
		Function: schema.FunctionCall{Name: "probe", Arguments: `{"x":1}`}}})
	// 场景1:弹回后改口给出终答 → 放行改口结果
	m1 := testmodel.New(call, schema.AssistantMessage("基于结果作答", nil))
	out, err := RepeatBreak(m1).Generate(ctx, []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "基于结果作答" || m1.Calls != 2 {
		t.Fatalf("bounce then comply: %v %+v calls=%d", err, out, m1.Calls)
	}

	// 场景2:弹回后仍发起同一调用 → 强制收束,引用缓存的真实结果
	m2 := testmodel.New(call, call)
	out, err = RepeatBreak(m2).Generate(ctx, []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 0 || !strings.Contains(out.Content, "result") || !strings.Contains(out.Content, "已终止") {
		t.Fatalf("forced finish must quote the cached result, got %+v", out)
	}

	// 场景3:非热点调用透传
	other := schema.AssistantMessage("", []schema.ToolCall{{ID: "c2", Type: "function",
		Function: schema.FunctionCall{Name: "probe", Arguments: `{"x":9}`}}})
	m3 := testmodel.New(other)
	out, err = RepeatBreak(m3).Generate(ctx, []*schema.Message{schema.UserMessage("q")})
	if err != nil || len(out.ToolCalls) != 1 || m3.Calls != 1 {
		t.Fatalf("non-hot call passes through: %v %+v", err, out)
	}
}

// TestObservedGenerate 验证内层模型调用的 callback 切面:handler 收到
// OnStart/OnEnd,RunInfo.Name 标注调用点;无 handler 时透传不炸。
func TestObservedGenerate(t *testing.T) {
	gen := func(_ context.Context, ms []*schema.Message) (*schema.Message, error) {
		return schema.AssistantMessage("内层结果", nil), nil
	}

	// 无 handler:透传
	out, err := observedGenerate(context.Background(), "digest/x", gen, nil)
	if err != nil || out.Content != "内层结果" {
		t.Fatalf("passthrough: %v %+v", err, out)
	}

	// 有 handler:OnStart/OnEnd 各一次,RunInfo.Name 可见
	var starts, ends int
	var seenName string
	h := callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			starts++
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackOutput) context.Context {
			ends++
			if info != nil {
				seenName = info.Name
			}
			return ctx
		}).Build()
	ctx := callbacks.InitCallbacks(context.Background(), &callbacks.RunInfo{Name: "outer"}, h)
	if _, err := observedGenerate(ctx, "finish-guard/bounce", gen, nil); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || ends != 1 || seenName != "finish-guard/bounce" {
		t.Fatalf("callbacks: starts=%d ends=%d name=%q", starts, ends, seenName)
	}
}
