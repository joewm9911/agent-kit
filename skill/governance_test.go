package skill

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// TestSkillInternalApprovalGate 验证治理下沉:skill 内部的 mutating
// 工具同样过审批闸门,不再是"skill 边界批准一次、内部裸奔"。
func TestSkillInternalApprovalGate(t *testing.T) {
	var executed int32
	write := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "fs", Name: "write_file"},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) {
		atomic.AddInt32(&executed, 1)
		return "written", nil
	})

	build := func() capability.Capability {
		m := testmodel.New(
			testmodel.ToolCallMsg("write_file", `{"input":"x"}`),
			schema.AssistantMessage("done", nil),
		)
		decl := &Declaration{
			Mode:   "subloop", // 夹具意图:隔离子循环(缺省已切 inline)
			Name:   "ops/writer",
			Prompt: prompt.Value{Literal: "写入 {input}"},
		}
		c, err := Build(context.Background(), decl, Deps{
			DefaultModel: m,
			Capabilities: []capability.Capability{write}, // 预解析注入
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// deny 模式:内部工具被拦,真实工具不执行
	sk := build()
	ctx := loop.WithApprovalMode(context.Background(), loop.ApprovalDeny)
	if _, err := capability.Invoke(ctx, sk, `{"input":"a"}`); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&executed) != 0 {
		t.Fatal("mutating tool inside skill should be blocked under deny mode")
	}

	// auto 模式:放行
	sk = build()
	ctx = loop.WithApprovalMode(context.Background(), loop.ApprovalAuto)
	if _, err := capability.Invoke(ctx, sk, `{"input":"a"}`); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&executed) != 1 {
		t.Fatal("mutating tool should execute under auto mode")
	}
}

// TestSkillInternalBudget 验证 skill 内部的模型调用计入调用方会话预算。
func TestSkillInternalBudget(t *testing.T) {
	read := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "read"},
	}, func(ctx context.Context, in string) (string, error) { return "data", nil })

	// 模型脚本:调工具 → 再调工具 → 收尾;预算只够 2 次模型调用
	m := loop.BudgetModel(testmodel.New(
		testmodel.ToolCallMsg("read", `{}`),
		testmodel.ToolCallMsg("read", `{}`),
		schema.AssistantMessage("summary", nil),
	))
	decl := &Declaration{
		Mode:   "subloop", // 夹具意图:隔离子循环(缺省已切 inline)
		Name:   "ops/reader",
		Prompt: prompt.Value{Literal: "读取 {input}"},
	}
	sk, err := Build(context.Background(), decl, Deps{
		DefaultModel: m,
		Capabilities: []capability.Capability{read},
	})
	if err != nil {
		t.Fatal(err)
	}

	gate := loop.NewBudgetGate(loop.BudgetConfig{MaxModelCalls: 2}, nil, 0)
	ctx := loop.WithBudget(runctx.With(context.Background(), "host", "sess-1"), gate)
	ctx = loop.WithApprovalMode(ctx, loop.ApprovalAuto)

	_, err = capability.Invoke(ctx, sk, `{"input":"x"}`)
	var exhausted *loop.ErrBudgetExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("skill internal calls should hit host session budget, got %v", err)
	}
	if calls, _ := gate.Spend(ctx); calls != 2 {
		t.Fatalf("spend = %d, want 2 (internal calls counted)", calls)
	}
}
