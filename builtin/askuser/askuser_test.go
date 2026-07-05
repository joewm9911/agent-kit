package askuser

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
)

type stubInteractor struct{ answer string }

func (s stubInteractor) Ask(context.Context, string) (string, error) { return s.answer, nil }
func (s stubInteractor) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return true, nil
}

func TestAskUserWithChannel(t *testing.T) {
	ctx := runctx.WithInteractor(context.Background(), stubInteractor{answer: "北京"})
	out, err := capability.Invoke(ctx, New(), `{"question":"哪个城市?"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "北京") {
		t.Fatalf("got %q", out)
	}
}

func TestAskUserWithoutChannel(t *testing.T) {
	// 无交互通道:以工具结果告知模型,不报错、不中断循环
	out, err := capability.Invoke(context.Background(), New(), `{"question":"?"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "没有用户交互通道") {
		t.Fatalf("got %q", out)
	}
}
