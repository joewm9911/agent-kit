package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func echoCap() capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
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

func TestPlanExecute(t *testing.T) {
	// 脚本:planner 出 1 步计划 → 执行器直接回答 → replanner 判定完成。
	m := testmodel.New(
		schema.AssistantMessage(`{"steps": ["查询天气"]}`, nil),
		schema.AssistantMessage("晴,25 度", nil),
		schema.AssistantMessage(`{"action": "finish", "response": "北京今天晴,25 度"}`, nil),
	)
	runner, err := Build(context.Background(), "plan-execute", &Assembly{
		Model:        m,
		Capabilities: []capability.Capability{echoCap()},
		Config:       map[string]any{"max_rounds": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Generate(context.Background(), []*schema.Message{schema.UserMessage("北京天气如何")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "25 度") {
		t.Fatalf("got %q", out.Content)
	}
}
