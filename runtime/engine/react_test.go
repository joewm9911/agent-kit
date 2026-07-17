package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"fmt"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func echoCap() capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "t", Name: "echo"},
		Description: "回显输入",
	}, func(ctx context.Context, argsJSON string) (string, error) {
		return "echo:" + capability.ParseSingle(argsJSON, "input"), nil
	})
}

func TestReActLoopWithTool(t *testing.T) {
	// 脚本:第一轮调用 echo 工具,第二轮给出最终回答。
	m := testmodel.New(
		testmodel.ToolCallMsg("echo", `{"input":"hi"}`),
		schema.AssistantMessage("最终回答", nil),
	)
	runner, err := Build(context.Background(), "react", &Assembly{
		Model:        m,
		Capabilities: []capability.Capability{echoCap()},
		MaxSteps:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "最终回答" {
		t.Fatalf("got %q", out.Content)
	}
	if m.Calls != 2 {
		t.Fatalf("want 2 model calls (tool round + final), got %d", m.Calls)
	}
}

func TestReActNoToolsFallsBackToBareModel(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("direct", nil))
	runner, err := Build(context.Background(), "react", &Assembly{Model: m})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "direct" {
		t.Fatalf("got %q", out.Content)
	}
}

// TestEngineCollapsed 验证引擎塌缩:范式引擎已删,注册表仅剩 react。
func TestEngineCollapsed(t *testing.T) {
	for _, name := range []string{"plan-execute", "rewoo", "reflection", "router", "direct", "graph", "workflow"} {
		if _, err := Build(context.Background(), name, &Assembly{Model: testmodel.New()}); err == nil {
			t.Fatalf("engine %q must be gone", name)
		}
	}
}

// TestReActMaxStepsMeansRounds:max_steps 的对外语义 = 工具调用轮数。
// 配 N 恰好允许 N 轮工具调用 + 一次收尾作答;N-1 则超限。
func TestReActMaxStepsMeansRounds(t *testing.T) {
	script := func() *testmodel.Fake {
		return testmodel.New(
			testmodel.ToolCallMsg("echo", `{"input":"1"}`),
			testmodel.ToolCallMsg("echo", `{"input":"2"}`),
			schema.AssistantMessage("完成", nil),
		)
	}

	// 2 轮工具调用,max_steps=2:恰好放行
	runner, err := Build(context.Background(), "react", &Assembly{
		Model: script(), Capabilities: []capability.Capability{echoCap()}, MaxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("go")})
	if err != nil || out.Content != "完成" {
		t.Fatalf("2 rounds under max_steps=2 must pass: %v %+v", err, out)
	}

	// max_steps=1:第二轮超限
	runner, err = Build(context.Background(), "react", &Assembly{
		Model: script(), Capabilities: []capability.Capability{echoCap()}, MaxSteps: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("go")}); err == nil ||
		!strings.Contains(err.Error(), "max steps") {
		t.Fatalf("2 rounds under max_steps=1 must exceed, got %v", err)
	}
}

// TestStreamToolCallChecker:先文本后工具调用的流被正确判定为工具调用;
// 纯文本流判定为终答。
func TestStreamToolCallChecker(t *testing.T) {
	feed := func(msgs ...*schema.Message) *schema.StreamReader[*schema.Message] {
		sr, sw := schema.Pipe[*schema.Message](len(msgs))
		for _, m := range msgs {
			sw.Send(m, nil)
		}
		sw.Close()
		return sr
	}

	got, err := streamToolCallChecker(context.Background(), feed(
		schema.AssistantMessage("我先说明一下,", nil),
		testmodel.ToolCallMsg("echo", `{}`),
	))
	if err != nil || !got {
		t.Fatalf("text-then-toolcall must be detected: %v %v", got, err)
	}

	got, err = streamToolCallChecker(context.Background(), feed(
		schema.AssistantMessage("这就是", nil),
		schema.AssistantMessage("最终回答。", nil),
	))
	if err != nil || got {
		t.Fatalf("pure text must be final answer: %v %v", got, err)
	}
}

// TestReActToolErrorAsResult:工具返回 error(如参数解析失败)不再炸轮——
// middleware 转成结果字符串,模型读错误自纠后正常收尾。
func TestReActToolErrorAsResult(t *testing.T) {
	fails := 0
	brittle := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "brittle"},
	}, func(_ context.Context, _ string) (string, error) {
		fails++
		return "", fmt.Errorf("parse args: unexpected end of JSON input")
	})
	m := testmodel.New(
		testmodel.ToolCallMsg("brittle", `{"broken`),
		schema.AssistantMessage("参数有误,已停止重试。", nil),
	)
	runner, err := Build(context.Background(), "react", &Assembly{
		Model: m, Capabilities: []capability.Capability{brittle}, MaxSteps: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("go")})
	if err != nil {
		t.Fatalf("tool error must not kill the turn: %v", err)
	}
	if out.Content != "参数有误,已停止重试。" || fails != 1 {
		t.Fatalf("got %q fails=%d", out.Content, fails)
	}
}
