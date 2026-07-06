package loop

// budget/approval 运行态落 store.KV 的跨副本一致性验收:两个 Gate/State
// 实例(模拟两个副本)共享同一 KV 后端,账目与决策记忆必须互通;
// 键按 (agent, session) 隔离,互不串扰。

import (
	"context"
	"errors"
	"testing"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
)

func TestBudgetCrossReplica(t *testing.T) {
	kv := store.NewInMemory()
	cfg := BudgetConfig{MaxModelCalls: 3}
	replicaA := NewBudgetGate(cfg, kv, 0)
	replicaB := NewBudgetGate(cfg, kv, 0)
	ctx := runctx.With(context.Background(), "ops", "s1")

	// 副本 A 记 2 次、副本 B 记 1 次:共享账目 = 3,任一副本第 4 次必拒。
	for i := 0; i < 2; i++ {
		if _, err := replicaA.beginCall(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := replicaB.beginCall(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := replicaB.beginCall(ctx)
	var exhausted *ErrBudgetExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("跨副本预算未共享账目(want ErrBudgetExhausted), got %v", err)
	}
	if calls, _ := replicaA.Spend(ctx); calls != 3 {
		t.Fatalf("shared spend = %d, want 3", calls)
	}

	// token 用量同样跨副本累计。
	replicaA.addTokens(ctx, 100)
	if _, tokens := replicaB.Spend(ctx); tokens != 100 {
		t.Fatalf("tokens = %d, want 100", tokens)
	}

	// 隔离:另一会话、另一 agent 各自从零开始。
	if _, err := replicaB.beginCall(runctx.With(context.Background(), "ops", "s2")); err != nil {
		t.Fatalf("other session must not be limited: %v", err)
	}
	if _, err := replicaB.beginCall(runctx.With(context.Background(), "ops2", "s1")); err != nil {
		t.Fatalf("other agent must not be limited: %v", err)
	}
}

func TestApprovalMemoryCrossReplica(t *testing.T) {
	kv := store.NewInMemory()
	policy := ApprovalPolicy{Remember: true}
	replicaA, err := NewApprovalState(ApprovalInteractive, policy, kv, 0)
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := NewApprovalState(ApprovalInteractive, policy, kv, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := runctx.With(context.Background(), "ops", "s1")

	// 副本 A 记住"总是允许",副本 B 直接命中,不再重复询问。
	replicaA.memorize(ctx, "tool.t/x/y", true)
	allowed, ok := replicaB.recall(ctx, "tool.t/x/y")
	if !ok || !allowed {
		t.Fatalf("decision memory not shared across replicas: %v %v", allowed, ok)
	}
	// "总是拒绝"同样互通。
	replicaB.memorize(ctx, "tool.t/x/z", false)
	if allowed, ok := replicaA.recall(ctx, "tool.t/x/z"); !ok || allowed {
		t.Fatalf("deny memory not shared: %v %v", allowed, ok)
	}

	// 隔离:另一会话不命中(决策记忆是会话级)。
	other := runctx.With(context.Background(), "ops", "s2")
	if _, ok := replicaB.recall(other, "tool.t/x/y"); ok {
		t.Fatal("decision memory must be session-scoped")
	}
}
