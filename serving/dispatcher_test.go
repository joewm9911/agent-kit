package serving

import (
	"context"
	"fmt"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// fakeChannel 记录所有 Send/Update,供断言。canUpdate=false 模拟
// 不支持消息更新的通道(card/stream 应退化为整段回复)。
type fakeChannel struct {
	mu        sync.Mutex
	sent      []string
	updated   []string // "<msgID>:<text>"
	canUpdate bool
}

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Start(context.Context, *http.ServeMux, channel.InboundHandler) error {
	return nil
}
func (f *fakeChannel) Send(_ context.Context, _ channel.ConvRef, msg channel.Outbound) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg.Text)
	return fmt.Sprintf("mid-%d", len(f.sent)), nil
}
func (f *fakeChannel) Update(_ context.Context, _ channel.ConvRef, msgID string, msg channel.Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canUpdate {
		return channel.ErrUpdateUnsupported
	}
	f.updated = append(f.updated, msgID+":"+msg.Text)
	return nil
}

func (f *fakeChannel) updates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updated...)
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
	return schema.AssistantMessage("已确认城市:"+ans, nil), nil
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

	conv := channel.ConvRef{Channel: "fake", Chat: "c1", User: "u1"}
	h(context.Background(), channel.Inbound{Conv: conv, Text: "帮我查天气", EventID: "e1"})

	// 等 agent 把提问发到会话里
	waitFor(t, func() bool { return len(fc.messages()) >= 1 })
	if !strings.Contains(fc.messages()[0], "哪个城市") {
		t.Fatalf("first message should be the question, got %q", fc.messages()[0])
	}

	// 用户在同一会话回复:应被路由给挂起的提问,而非触发新一轮运行
	h(context.Background(), channel.Inbound{Conv: conv, Text: "北京", EventID: "e2"})

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

// TestSessionKeyThread 验证话题会话映射:话题消息按 thread 细分,
// chat_user 在话题之上再叠加用户隔离;无话题行为不变。
func TestSessionKeyThread(t *testing.T) {
	d := NewDispatcher(nil)
	plain := channel.ConvRef{Channel: "feishu", Chat: "oc_1", User: "u1"}
	topic := channel.ConvRef{Channel: "feishu", Chat: "oc_1", User: "u1", Thread: "omt_9", Anchor: "om_5"}

	if got := d.sessionKey(Binding{}, plain); got != "feishu-oc_1" {
		t.Fatalf("plain chat key = %q", got)
	}
	if got := d.sessionKey(Binding{}, topic); got != "feishu-oc_1-omt_9" {
		t.Fatalf("topic key = %q", got)
	}
	if got := d.sessionKey(Binding{SessionMapping: "chat_user"}, topic); got != "feishu-oc_1-omt_9-u1" {
		t.Fatalf("topic+user key = %q", got)
	}
	// 同群不同话题必须是不同会话
	other := topic
	other.Thread = "omt_10"
	if d.sessionKey(Binding{}, topic) == d.sessionKey(Binding{}, other) {
		t.Fatal("different threads must map to different sessions")
	}
}

// echoRunner:直接回答的最小 runner。
type echoRunner struct{}

func (echoRunner) Generate(context.Context, []*schema.Message) (*schema.Message, error) {
	return schema.AssistantMessage("最终答案", nil), nil
}
func (r echoRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestCardReplyMode:card 模式先发"处理中"占位,完成后原地更新为答案;
// 通道不支持更新时退化为整段回复(占位之外再发一条)。
func TestCardReplyMode(t *testing.T) {
	// 支持更新的通道:占位 + 原地更新,不再追加消息
	fc := &fakeChannel{canUpdate: true}
	ag := agent.New("a", "", echoRunner{}, nil, agent.Options{})
	d := NewDispatcher(nil)
	h := d.Handler(Binding{Channel: fc, Agent: ag, ReplyMode: "card"})
	h(context.Background(), channel.Inbound{Conv: channel.ConvRef{Channel: "fake", Chat: "c9"}, Text: "问题", EventID: "ec1"})

	waitFor(t, func() bool { return len(fc.updates()) >= 1 })
	if msgs := fc.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "处理中") {
		t.Fatalf("placeholder expected, got %v", msgs)
	}
	if ups := fc.updates(); !strings.Contains(ups[0], "mid-1:最终答案") {
		t.Fatalf("in-place update expected, got %v", ups)
	}

	// 不支持更新的通道:退化为整段回复
	fc2 := &fakeChannel{}
	d2 := NewDispatcher(nil)
	h2 := d2.Handler(Binding{Channel: fc2, Agent: ag, ReplyMode: "card"})
	h2(context.Background(), channel.Inbound{Conv: channel.ConvRef{Channel: "fake", Chat: "c9"}, Text: "问题", EventID: "ec2"})

	waitFor(t, func() bool { return len(fc2.messages()) >= 2 })
	if msgs := fc2.messages(); msgs[len(msgs)-1] != "最终答案" {
		t.Fatalf("fallback full reply expected, got %v", msgs)
	}
}
