package loop

import (
	"context"
	"errors"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
)

// TestReviewGlobalBudget:永不满足的评审器只能烧全局预算——总生成次数
// 封顶 1+reviewMaxRetries,不再有旧守卫链的乘法放大;耗尽后放行。
func TestReviewGlobalBudget(t *testing.T) {
	m := testmodel.New() // 永远返回 "done"
	always := func(_ context.Context, a Attempt) Verdict {
		return Verdict{Action: Retry, Reason: "nag-a", Append: []*schema.Message{a.Out, schema.SystemMessage("再来")}}
	}
	also := func(_ context.Context, a Attempt) Verdict {
		return Verdict{Action: Retry, Reason: "nag-b", Append: []*schema.Message{a.Out, schema.SystemMessage("还来")}}
	}
	out, err := ReviewModel(m, always, also).Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "done" {
		t.Fatalf("exhausted budget must pass through: %v %+v", err, out)
	}
	if m.Calls != 1+reviewMaxRetries {
		t.Fatalf("calls = %d, want %d(全局预算,两个评审器共享)", m.Calls, 1+reviewMaxRetries)
	}
}

// TestReviewForceSkipsFurtherReview:Force 直接收束,不再评审、不再生成。
func TestReviewForceSkipsFurtherReview(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("原始输出", nil))
	forced := schema.AssistantMessage("强制收束", nil)
	var afterCalled bool
	first := func(context.Context, Attempt) Verdict {
		return Verdict{Action: Force, Reason: "f", Replace: forced}
	}
	after := func(context.Context, Attempt) Verdict {
		afterCalled = true
		return Verdict{}
	}
	out, err := ReviewModel(m, first, after).Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out != forced || m.Calls != 1 {
		t.Fatalf("force: %v %+v calls=%d", err, out, m.Calls)
	}
	if afterCalled {
		t.Fatal("Force 生效后不得继续征询后续评审器")
	}
}

// TestReviewRewrite:Rewrite 整体替换下次输入(改写输入重试)。
func TestReviewRewrite(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("第一次", nil),
		schema.AssistantMessage("第二次", nil),
	)
	rewrote := false
	r := func(_ context.Context, a Attempt) Verdict {
		if a.Tally("rw") > 0 {
			if len(a.Msgs) != 1 || a.Msgs[0].Content != "精简后的输入" {
				t.Fatalf("rewrite 未生效,msgs=%+v", a.Msgs)
			}
			rewrote = true
			return Verdict{}
		}
		return Verdict{Action: Retry, Reason: "rw",
			Rewrite: []*schema.Message{schema.UserMessage("精简后的输入")}}
	}
	out, err := ReviewModel(m, r).Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("很长很长的原始输入")})
	if err != nil || out.Content != "第二次" || !rewrote {
		t.Fatalf("rewrite retry: %v %+v rewrote=%v", err, out, rewrote)
	}
}

// TestReviewTerminalErrorPassthrough:轮次终止级错误透传,不进评审。
func TestReviewTerminalErrorPassthrough(t *testing.T) {
	reviewed := false
	r := func(context.Context, Attempt) Verdict {
		reviewed = true
		return Verdict{Action: Retry, Reason: "x"}
	}
	term := &ErrBudgetExhausted{}
	m := &errModel{err: term}
	_, err := ReviewModel(m, r).Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if !errors.As(err, new(*ErrBudgetExhausted)) {
		t.Fatalf("terminal error must pass through, got %v", err)
	}
	if reviewed {
		t.Fatal("terminal error 不得进入评审")
	}
	if m.calls != 1 {
		t.Fatalf("calls=%d", m.calls)
	}
}

// TestReviewComposedChain:组合装配(repeat→finish→checked)端到端——
// 伪调用被 FinishReviewer 弹回、check 收口再弹一次、最终放行,总预算内。
func TestReviewComposedChain(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("```\nfunctions.todo_write({})\n```", nil), // finish 弹回
		schema.AssistantMessage("做完了。", nil),                               // check 弹回一次
		schema.AssistantMessage("最终回答", nil),
	)
	nagged := false
	check := func(context.Context) string {
		if nagged {
			return ""
		}
		nagged = true
		return "[收口] 先核对清单。"
	}
	rm := ReviewModel(m, RepeatBreakReviewer(), FinishReviewer(), CheckedReviewer(check))
	out, err := rm.Generate(context.Background(), []*schema.Message{schema.UserMessage("干活")})
	if err != nil || out.Content != "最终回答" || m.Calls != 3 {
		t.Fatalf("composed chain: %v %+v calls=%d", err, out, m.Calls)
	}
}

// errModel 是恒返回指定错误的最小模型假件。
type errModel struct {
	err   error
	calls int
}

func (e *errModel) Generate(context.Context, []*schema.Message, ...einomodel.Option) (*schema.Message, error) {
	e.calls++
	return nil, e.err
}

func (e *errModel) Stream(context.Context, []*schema.Message, ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, e.err
}

func (e *errModel) WithTools([]*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return e, nil
}
