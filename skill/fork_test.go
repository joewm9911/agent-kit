package skill

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// echoInputModel 记录收到的消息并返回固定回答,用于验证 fork 起始消息。
type echoInputModel struct {
	mu   sync.Mutex
	seen [][]*schema.Message
}

func (m *echoInputModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.seen = append(m.seen, msgs)
	m.mu.Unlock()
	return schema.AssistantMessage("done", nil), nil
}

func (m *echoInputModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	out, _ := m.Generate(ctx, in, opts...)
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

func (m *echoInputModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// TestGraphStepFork 验证 context: fork 的步骤让 component 内部循环
// 以调用方对话快照起步;未声明的步骤保持从零起步。
func TestGraphStepFork(t *testing.T) {
	em := &echoInputModel{}
	comp, err := Build(context.Background(), &Declaration{
		Name:   "t/analyzer",
		Prompt: prompt.Value{Literal: "分析 {q}"},
	}, Deps{DefaultModel: em})
	if err != nil {
		t.Fatal(err)
	}

	resolve := func(use string) (capability.Capability, error) { return comp, nil }
	sk, err := engine.BuildGraph(context.Background(), &engine.GraphDeclaration{
		Name: "probe",
		Steps: []engine.Step{
			{Name: "fresh", Use: "c", Args: prompt.Value{Literal: `{"q":"a"}`}},
			{Name: "forked", Use: "c", Args: prompt.Value{Literal: `{"q":"b"}`}, Context: "fork"},
		},
	}, "ns", resolve)
	if err != nil {
		t.Fatal(err)
	}

	snap := []*schema.Message{schema.UserMessage("背景:payment 服务上周也故障过")}
	ctx := loop.WithConversationSnapshot(context.Background(), snap)
	if _, err := capability.Invoke(ctx, sk, `{}`); err != nil {
		t.Fatal(err)
	}

	if len(em.seen) != 2 {
		t.Fatalf("model invoked %d times", len(em.seen))
	}
	// 第一步 fresh:L1 规约 + 任务书,不含快照
	fresh := em.seen[0]
	if len(fresh) != 2 || !strings.Contains(fresh[1].Content, "分析 a") {
		t.Fatalf("fresh step msgs = %d: %+v", len(fresh), fresh)
	}
	for _, m := range fresh {
		if strings.Contains(m.Content, "payment") {
			t.Fatal("fresh step should not see the snapshot")
		}
	}
	// 第二步 fork:L1 规约 + 背景标注 + 快照 + 任务书
	forked := em.seen[1]
	if len(forked) != 4 {
		t.Fatalf("forked step msgs = %d, want 4", len(forked))
	}
	if !strings.Contains(forked[2].Content, "payment") || !strings.Contains(forked[3].Content, "分析 b") {
		t.Fatalf("forked shape wrong: %+v", forked)
	}
}

func TestGraphStepBadContext(t *testing.T) {
	c := testCap("a", func(_ context.Context, s string) (string, error) { return s, nil })
	_, err := engine.BuildGraph(context.Background(), &engine.GraphDeclaration{
		Name:  "bad",
		Steps: []engine.Step{{Name: "s", Use: "a", Context: "clone"}},
	}, "ns", resolverFor(map[string]capability.Capability{"a": c}))
	if err == nil || !strings.Contains(err.Error(), "bad context") {
		t.Fatalf("expect context validation error, got %v", err)
	}
}
