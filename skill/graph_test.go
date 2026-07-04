package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/runctx"
)

// testCap 构造一个记录输入并返回 transform(输入) 的能力。
func testCap(name string, fn func(ctx context.Context, args string) (string, error)) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: name},
	}, fn)
}

func resolverFor(caps map[string]capability.Capability) StepResolver {
	return func(use string) (capability.Capability, error) {
		if c, ok := caps[use]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("unknown use %q", use)
	}
}

func TestGraphSerialDefaultChain(t *testing.T) {
	var order []string
	var mu sync.Mutex
	mk := func(name string) capability.Capability {
		return testCap(name, func(_ context.Context, args string) (string, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return name + "(" + args + ")", nil
		})
	}
	caps := map[string]capability.Capability{"a": mk("a"), "b": mk("b"), "c": mk("c")}

	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:   "chain",
		Params: map[string]ParamDecl{"input": {Type: "string", Required: true}},
		Steps: []Step{
			{Name: "s1", Use: "a", Args: "{input}"},
			{Name: "s2", Use: "b", Args: "{s1}"},
			{Name: "s3", Use: "c", Args: "{s2}"},
		},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{"input":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "c(b(a(x)))" {
		t.Fatalf("got %q", out)
	}
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("order = %v", order)
	}
	if ref := sk.Meta().Ref.String(); ref != "cap://skill/ns/chain" {
		t.Fatalf("ref = %s", ref)
	}
}

func TestGraphParallelAndJoin(t *testing.T) {
	var running, peak int32
	slow := func(name string) capability.Capability {
		return testCap(name, func(_ context.Context, args string) (string, error) {
			n := atomic.AddInt32(&running, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&running, -1)
			return name, nil
		})
	}
	join := testCap("join", func(_ context.Context, args string) (string, error) {
		return "joined:" + args, nil
	})
	caps := map[string]capability.Capability{"l": slow("L"), "r": slow("R"), "j": join}

	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:   "fanout",
		Params: map[string]ParamDecl{"q": {Type: "string"}},
		Steps: []Step{
			{Name: "root", Use: "l", Needs: []string{}, Args: "{q}"},
			{Name: "left", Use: "l", Needs: []string{"root"}, Args: "{q}"},
			{Name: "right", Use: "r", Needs: []string{"root"}, Args: "{q}"},
			{Name: "join", Use: "j", Needs: []string{"left", "right"}, Args: "{left}+{right}"},
		},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{"q":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "joined:L+R" {
		t.Fatalf("got %q", out)
	}
	if atomic.LoadInt32(&peak) < 2 {
		t.Fatalf("left/right should run concurrently, peak = %d", peak)
	}
}

func TestGraphCycleRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	_, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name: "cyc",
		Steps: []Step{
			{Name: "s1", Use: "a", Needs: []string{"s2"}},
			{Name: "s2", Use: "a", Needs: []string{"s1"}},
		},
	}, "ns", resolverFor(caps))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expect cycle error, got %v", err)
	}
}

func TestGraphUnknownNeedRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	_, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "bad",
		Steps: []Step{{Name: "s1", Use: "a", Needs: []string{"ghost"}}},
	}, "ns", resolverFor(caps))
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("expect unknown-step error, got %v", err)
	}
}

func TestGraphTemplateOutsideClosureRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	// s2 与 s3 并行(都只依赖 s1),s3 引用 {s2} 是竞态 → 拒绝装配
	_, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name: "race",
		Steps: []Step{
			{Name: "s1", Use: "a"},
			{Name: "s2", Use: "a", Needs: []string{"s1"}},
			{Name: "s3", Use: "a", Needs: []string{"s1"}, Args: "{s2}"},
		},
	}, "ns", resolverFor(caps))
	if err == nil || !strings.Contains(err.Error(), "needs closure") {
		t.Fatalf("expect closure error, got %v", err)
	}
}

func TestGraphUnknownPlaceholderRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	_, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "typo",
		Steps: []Step{{Name: "s1", Use: "a", Args: "{tokn}"}},
	}, "ns", resolverFor(caps))
	if err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Fatalf("expect placeholder error, got %v", err)
	}
}

func TestGraphStepFailureInterrupts(t *testing.T) {
	var ran int32
	caps := map[string]capability.Capability{
		"boom": testCap("boom", func(_ context.Context, s string) (string, error) {
			return "", fmt.Errorf("auth denied")
		}),
		"next": testCap("next", func(_ context.Context, s string) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "ok", nil
		}),
	}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name: "guard",
		Steps: []Step{
			{Name: "auth", Use: "boom"},
			{Name: "run", Use: "next"},
		},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	_, err = capability.Invoke(context.Background(), sk, `{}`)
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expect step failure, got %v", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("downstream step should not run after failure")
	}
}

func TestGraphStepRetry(t *testing.T) {
	var calls int32
	caps := map[string]capability.Capability{
		"flaky": testCap("flaky", func(_ context.Context, s string) (string, error) {
			if atomic.AddInt32(&calls, 1) < 3 {
				return "", fmt.Errorf("transient")
			}
			return "ok", nil
		}),
	}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "retry",
		Steps: []Step{{Name: "s1", Use: "flaky", Retry: 2}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{}`)
	if err != nil || out != "ok" {
		t.Fatalf("got %q %v (calls=%d)", out, err, calls)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestGraphStepTimeout(t *testing.T) {
	caps := map[string]capability.Capability{
		"hang": testCap("hang", func(ctx context.Context, s string) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "late", nil
			}
		}),
	}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "slow",
		Steps: []Step{{Name: "s1", Use: "hang", Timeout: loop.Duration(50 * time.Millisecond)}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	_, err = capability.Invoke(context.Background(), sk, `{}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expect timeout error, got %v", err)
	}
}

func TestGraphMissingRequiredParam(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:   "strict",
		Params: map[string]ParamDecl{"token": {Type: "string", Required: true}},
		Steps:  []Step{{Name: "s1", Use: "a", Args: "{token}"}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{}`)
	if err != nil {
		t.Fatalf("missing param should return message, not error: %v", err)
	}
	if !strings.Contains(out, "token") {
		t.Fatalf("got %q", out)
	}
}

func TestGraphStateIsolationAcrossInvokes(t *testing.T) {
	echo := testCap("echo", func(_ context.Context, s string) (string, error) { return s, nil })
	caps := map[string]capability.Capability{"e": echo}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:   "iso",
		Params: map[string]ParamDecl{"v": {Type: "string"}},
		Steps:  []Step{{Name: "s1", Use: "e", Args: "{v}"}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, _ := json.Marshal(map[string]string{"v": fmt.Sprintf("v%d", i)})
			out, err := capability.Invoke(context.Background(), sk, string(in))
			if err != nil || out != fmt.Sprintf("v%d", i) {
				t.Errorf("invoke %d: got %q %v", i, out, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestGraphRiskPropagation(t *testing.T) {
	mut := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "t", Name: "write"},
		Risk: capability.RiskMutating,
	}, func(_ context.Context, s string) (string, error) { return "w", nil })
	caps := map[string]capability.Capability{"w": mut}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "risky",
		Steps: []Step{{Name: "s1", Use: "w"}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Meta().Risk != capability.RiskMutating {
		t.Fatalf("risk = %v, want mutating", sk.Meta().Risk)
	}
}

// TestGraphJSONEscapeInStringContext 验证渲染的 JSON 安全:占位符在
// JSON 字符串内时值自动转义(上游输出含引号不破坏下游解析),在
// 字符串外(纯文本提示词)原样注入。
func TestGraphJSONEscapeInStringContext(t *testing.T) {
	quoteOut := testCap("q", func(_ context.Context, s string) (string, error) {
		return `he said "hi"` + "\n{done}", nil // 含引号、换行、花括号
	})
	parse := testCap("p", func(_ context.Context, s string) (string, error) {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return "", fmt.Errorf("downstream got invalid JSON: %v", err)
		}
		return fmt.Sprint(m["text"]), nil
	})
	caps := map[string]capability.Capability{"q": quoteOut, "p": parse}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name: "esc",
		Steps: []Step{
			{Name: "raw", Use: "q"},
			{Name: "next", Use: "p", Args: `{"text":"{raw}"}`}, // 字符串上下文 → 转义
		},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `he said "hi"`) {
		t.Fatalf("value should round-trip through JSON, got %q", out)
	}

	// 字符串外(纯文本模板):原样注入,不转义
	echo := testCap("e", func(_ context.Context, s string) (string, error) { return s, nil })
	caps2 := map[string]capability.Capability{"q": quoteOut, "e": echo}
	sk2, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name: "plain",
		Steps: []Step{
			{Name: "raw", Use: "q"},
			{Name: "say", Use: "e", Args: "总结:{raw}"},
		},
	}, "ns", resolverFor(caps2))
	if err != nil {
		t.Fatal(err)
	}
	out, err = capability.Invoke(context.Background(), sk2, `{}`)
	if err != nil || !strings.Contains(out, "he said \"hi\"\n") {
		t.Fatalf("plain-text template should inject raw: %q %v", out, err)
	}
}

func TestGraphBuiltinInputVar(t *testing.T) {
	echo := testCap("echo", func(_ context.Context, s string) (string, error) { return s, nil })
	caps := map[string]capability.Capability{"e": echo}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "raw",
		Steps: []Step{{Name: "s1", Use: "e", Args: `{"q":"{$input}"}`}},
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}

	// {$input} 由框架从 runctx 直取,不经主脑转写
	ctx := runctx.WithInput(context.Background(), "北京明天适合出门吗?带伞不?")
	out, err := capability.Invoke(ctx, sk, `{}`)
	if err != nil || out != `{"q":"北京明天适合出门吗?带伞不?"}` {
		t.Fatalf("got %q %v", out, err)
	}

	// 调用方传入同名键不能顶掉保留变量
	out, err = capability.Invoke(ctx, sk, `{"$input":"劫持"}`)
	if err != nil || !strings.Contains(out, "带伞") {
		t.Fatalf("builtin var must not be overridable: %q %v", out, err)
	}

	// ctx 无输入(离线批处理等):替换为空串,确定性行为
	out, err = capability.Invoke(context.Background(), sk, `{}`)
	if err != nil || out != `{"q":""}` {
		t.Fatalf("no input should render empty: %q %v", out, err)
	}
}

func TestGraphUnknownBuiltinVarRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	_, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "typo",
		Steps: []Step{{Name: "s1", Use: "a", Args: "{$history}"}},
	}, "ns", resolverFor(caps))
	if err == nil || !strings.Contains(err.Error(), "unknown builtin variable") {
		t.Fatalf("expect builtin var error, got %v", err)
	}
}

func TestGraphDollarNamesRejected(t *testing.T) {
	caps := map[string]capability.Capability{"a": testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })}
	if _, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "badstep",
		Steps: []Step{{Name: "$s", Use: "a"}},
	}, "ns", resolverFor(caps)); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expect reserved-name error for step, got %v", err)
	}
	if _, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:   "badparam",
		Params: map[string]ParamDecl{"$input": {Type: "string"}},
		Steps:  []Step{{Name: "s", Use: "a"}},
	}, "ns", resolverFor(caps)); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expect reserved-name error for param, got %v", err)
	}
}

func TestGraphPassthroughEmptyArgs(t *testing.T) {
	echo := testCap("echo", func(_ context.Context, s string) (string, error) { return "got:" + s, nil })
	caps := map[string]capability.Capability{"e": echo}
	sk, err := BuildGraph(context.Background(), &GraphDeclaration{
		Name:  "pass",
		Steps: []Step{{Name: "s1", Use: "e"}}, // args 为空 → 透传原始入参
	}, "ns", resolverFor(caps))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{"any":"thing"}`)
	if err != nil || out != `got:{"any":"thing"}` {
		t.Fatalf("got %q %v", out, err)
	}
}
