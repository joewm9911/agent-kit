package channel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/runctx"
)

// fakeChannel 记录所有 Send,供断言。
type fakeChannel struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Start(context.Context, *http.ServeMux, InboundHandler) error {
	return nil
}
func (f *fakeChannel) Send(_ context.Context, _ ConvRef, msg Outbound) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg.Text)
	return "mid", nil
}
func (f *fakeChannel) Update(context.Context, ConvRef, string, Outbound) error {
	return ErrUpdateUnsupported
}

func (f *fakeChannel) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// askRunner 模拟"大脑需要补充信息":先通过交互通道提问,再基于回答作答。
type askRunner struct{}

func (askRunner) Generate(ctx context.Context, _ []*schema.Message) (*schema.Message, error) {
	it := runctx.GetInteractor(ctx)
	ans, err := it.Ask(ctx, "请问是哪个城市?")
	if err != nil {
		return nil, err
	}
	return schema.AssistantMessage("已确认城市:" + ans, nil), nil
}

func (r askRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestIMPendingQuestionRouting 验证 IM 桥接的核心语义:
// agent 提问后会话挂起,该会话的下一条用户消息作为答案送回、而不是开新一轮。
func TestIMPendingQuestionRouting(t *testing.T) {
	fc := &fakeChannel{}
	ag := agent.New("a", "", askRunner{}, nil, agent.Options{})
	d := NewDispatcher(nil)
	h := d.Handler(Binding{Channel: fc, Agent: ag})

	conv := ConvRef{Channel: "fake", Chat: "c1", User: "u1"}
	h(context.Background(), Inbound{Conv: conv, Text: "帮我查天气", EventID: "e1"})

	// 等 agent 把提问发到会话里
	waitFor(t, func() bool { return len(fc.messages()) >= 1 })
	if !strings.Contains(fc.messages()[0], "哪个城市") {
		t.Fatalf("first message should be the question, got %q", fc.messages()[0])
	}

	// 用户在同一会话回复:应被路由给挂起的提问,而非触发新一轮运行
	h(context.Background(), Inbound{Conv: conv, Text: "北京", EventID: "e2"})

	waitFor(t, func() bool { return len(fc.messages()) >= 2 })
	if !strings.Contains(fc.messages()[1], "已确认城市:北京") {
		t.Fatalf("final answer should use the reply, got %q", fc.messages()[1])
	}
}

// TestEventDedup 验证平台重试投递的幂等去重。
func TestEventDedup(t *testing.T) {
	d := NewDispatcher(nil)
	if d.duplicate("e1") {
		t.Fatal("first delivery should pass")
	}
	if !d.duplicate("e1") {
		t.Fatal("retry delivery should be deduped")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
