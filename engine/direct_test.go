package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func TestDirectWithToolRound(t *testing.T) {
	var executed int32
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
	}, func(_ context.Context, args string) (string, error) {
		atomic.AddInt32(&executed, 1)
		return "echo:" + args, nil
	})

	// 脚本:首次调用发起工具调用,收尾调用给最终回答
	m := testmodel.New(
		testmodel.ToolCallMsg("echo", `{"input":"hi"}`),
		schema.AssistantMessage("最终回答", nil),
	)
	r, err := Build(context.Background(), "direct", &Assembly{
		Model: m, Capabilities: []capability.Capability{echo},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("做事")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "最终回答" {
		t.Fatalf("got %q", out.Content)
	}
	if executed != 1 {
		t.Fatalf("tool executed %d times, want 1", executed)
	}
	// 固定两次模型调用:发起 + 收尾,没有循环
	if m.Calls != 2 {
		t.Fatalf("model calls = %d, want 2", m.Calls)
	}
}

func TestDirectAnswersWithoutTool(t *testing.T) {
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
	}, func(_ context.Context, args string) (string, error) { return "x", nil })
	m := testmodel.New(schema.AssistantMessage("直接作答", nil))
	r, err := Build(context.Background(), "direct", &Assembly{
		Model: m, Capabilities: []capability.Capability{echo},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "直接作答" {
		t.Fatalf("got %v %v", out, err)
	}
	if m.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (no tool round)", m.Calls)
	}
}

func TestDirectToolErrorFlowsToWrapUp(t *testing.T) {
	boom := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "boom"},
	}, func(_ context.Context, args string) (string, error) {
		return "", context.DeadlineExceeded
	})
	m := testmodel.New(
		testmodel.ToolCallMsg("boom", `{}`),
		schema.AssistantMessage("已说明失败", nil),
	)
	r, err := Build(context.Background(), "direct", &Assembly{
		Model: m, Capabilities: []capability.Capability{boom},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatalf("tool error should flow to wrap-up, not fail the run: %v", err)
	}
	if !strings.Contains(out.Content, "失败") {
		t.Fatalf("got %q", out.Content)
	}
}
