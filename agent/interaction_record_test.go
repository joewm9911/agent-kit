package agent

// ask_user 问答落会话(子循环内的问答不再随子循环丢弃)。

import (
	"context"
	"strings"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/askuser"
	"github.com/joewm9911/agent-kit/impl/session/inmemory"
)

// askThenAnswerRunner 模拟"子循环里问了用户一个问题"的一轮。
type askThenAnswerRunner struct{ ask capability.Capability }

func (r askThenAnswerRunner) Generate(ctx context.Context, _ []*schema.Message) (*schema.Message, error) {
	if _, err := capability.Invoke(ctx, r.ask, `{"question":"补货到哪个仓库?"}`); err != nil {
		return nil, err
	}
	return schema.AssistantMessage("已按用户指定的仓库处理。", nil), nil
}
func (r askThenAnswerRunner) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, in)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

type fixedInteractor struct{ answer string }

func (f fixedInteractor) Ask(context.Context, string) (string, error) { return f.answer, nil }
func (f fixedInteractor) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return true, nil
}

var _ einomodel.ToolCallingChatModel = nil // 保持导入整洁

func TestInteractionRecordedInSession(t *testing.T) {
	store := inmemory.New(0)
	ag := New("a", "", askThenAnswerRunner{ask: askuser.New()}, nil,
		Options{Store: store, Interactor: fixedInteractor{answer: "仓A"}})

	if _, err := ag.Run(context.Background(), "s1", "给 P103 补货"); err != nil {
		t.Fatal(err)
	}
	msgs, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "[用户交互记录]") &&
			strings.Contains(m.Content, "补货到哪个仓库") && strings.Contains(m.Content, "仓A") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("session must carry the ask_user Q&A, got %d msgs", len(msgs))
	}
}
