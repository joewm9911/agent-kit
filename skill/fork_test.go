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

// TestSubagentContextFork 验证声明级 context: fork 让 sub-agent 内部循环
// 以调用方对话快照起步;缺省(fresh)保持从零起步。
func TestSubagentContextFork(t *testing.T) {
	build := func(em *echoInputModel, ctxMode string) capability.Capability {
		c, err := BuildAgent(context.Background(), &AgentDecl{
			Name:    "t/analyzer",
			Context: ctxMode,
			Params:  map[string]capability.ParamDecl{"q": {Type: "string"}},
			Prompt:  prompt.Value{Literal: "分析 {q}"},
		}, Deps{DefaultModel: em})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	snap := []*schema.Message{schema.UserMessage("背景:payment 服务上周也故障过")}
	ctx := loop.WithConversationSnapshot(context.Background(), snap)

	// fresh(缺省):L1 规约 + 任务书,不含快照
	em := &echoInputModel{}
	if _, err := capability.Invoke(ctx, build(em, ""), `{"q":"a"}`); err != nil {
		t.Fatal(err)
	}
	if len(em.seen) != 1 {
		t.Fatalf("model invoked %d times", len(em.seen))
	}
	fresh := em.seen[0]
	if len(fresh) != 2 || !strings.Contains(fresh[1].Content, "分析 a") {
		t.Fatalf("fresh msgs = %d: %+v", len(fresh), fresh)
	}
	for _, m := range fresh {
		if strings.Contains(m.Content, "payment") {
			t.Fatal("fresh sub-agent should not see the snapshot")
		}
	}

	// fork:L1 规约 + 背景标注 + 快照 + 任务书
	em2 := &echoInputModel{}
	if _, err := capability.Invoke(ctx, build(em2, "fork"), `{"q":"b"}`); err != nil {
		t.Fatal(err)
	}
	forked := em2.seen[0]
	if len(forked) != 4 {
		t.Fatalf("forked msgs = %d, want 4", len(forked))
	}
	if !strings.Contains(forked[2].Content, "payment") || !strings.Contains(forked[3].Content, "分析 b") {
		t.Fatalf("forked shape wrong: %+v", forked)
	}
}

func TestSubagentBadContext(t *testing.T) {
	_, err := BuildAgent(context.Background(), &AgentDecl{
		Name: "t/bad", Context: "clone",
		Prompt: prompt.Value{Literal: "p"},
	}, Deps{DefaultModel: &echoInputModel{}})
	if err == nil || !strings.Contains(err.Error(), "unknown context") {
		t.Fatalf("expect context validation error, got %v", err)
	}
}
