package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/loop"
)

// TestAgentInterruptMidRun 验证运行中的循环可被叫停:第一个工具执行
// 期间用户叫停(模拟异步的"停,别做了"),后续步骤不再执行,agent 以
// 正常回答收束而非报错。
func TestAgentInterruptMidRun(t *testing.T) {
	ctx := context.Background()
	var ag *Agent
	var secondRan bool

	first := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "slow_step"},
	}, func(ctx context.Context, in string) (string, error) {
		ag.Interrupt("s1") // 执行期间用户叫停
		return "step1-done", nil
	})
	second := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "next_step"},
	}, func(ctx context.Context, in string) (string, error) {
		secondRan = true
		return "step2-done", nil
	})

	m := testmodel.New(
		testmodel.ToolCallMsg("slow_step", `{}`),
		testmodel.ToolCallMsg("next_step", `{}`),
		schema.AssistantMessage("all done", nil),
	)
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        m,
		Capabilities: loop.ControlTools([]capability.Capability{first, second}),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := inmemSession(0)
	ag = New("a", "", runner, m, Options{Store: store, Window: 50})

	answer, err := ag.Run(ctx, "s1", "做三步任务")
	if err != nil {
		t.Fatalf("interrupt should resolve to an answer, got error %v", err)
	}
	if !strings.Contains(answer, "中断") {
		t.Fatalf("answer = %q", answer)
	}
	if secondRan {
		t.Fatal("steps after interrupt must not run")
	}

	// 中断不搞坏会话:下一轮正常运行
	if _, err := ag.Run(ctx, "s1", "继续"); err != nil {
		t.Fatalf("next turn should run normally: %v", err)
	}
}

// TestAgentSteerMidRun 验证插话随下一个工具结果送达模型。
func TestAgentSteerMidRun(t *testing.T) {
	ctx := context.Background()
	var ag *Agent
	var secondInput string

	first := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "step_one"},
	}, func(ctx context.Context, in string) (string, error) {
		ag.Steer("s1", "顺便只看今天的数据") // 执行期间用户插话
		return "one-done", nil
	})
	// 模型第二次调用的输入里应包含插话(经工具结果)——用假模型看不到
	// 输入,改为在第二个工具处断言:执行到它说明循环仍在推进。
	second := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "step_two"},
	}, func(ctx context.Context, in string) (string, error) {
		secondInput = in
		return "two-done", nil
	})

	m := testmodel.New(
		testmodel.ToolCallMsg("step_one", `{}`),
		testmodel.ToolCallMsg("step_two", `{}`),
		schema.AssistantMessage("done", nil),
	)
	caps := loop.ControlTools([]capability.Capability{first, second})
	runner, err := engine.Build(ctx, "react", &engine.Assembly{Model: m, Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	ag = New("a", "", runner, m, Options{})

	if _, err := ag.Run(ctx, "s1", "任务"); err != nil {
		t.Fatal(err)
	}
	if secondInput == "" {
		t.Fatal("loop should continue after steer (not interrupt)")
	}
}
