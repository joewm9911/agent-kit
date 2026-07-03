package channel

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/runctx"
)

// Binding 把一个 Channel 路由到一个 Agent。
type Binding struct {
	Channel Channel
	Agent   *agent.Agent
	// SessionMapping:chat(群共享会话)| chat_user(群内每人独立会话)。
	SessionMapping string
	// ReplyMode:text(整段回复)| stream(先发占位,流式刷新,需通道支持 Update)。
	ReplyMode string
	// AskTimeout 是 ask_user / 审批等待用户回复的超时,默认 10 分钟。
	AskTimeout time.Duration
}

// Dispatcher 承接所有 Binding 的消息分发:
//   - 同会话串行、跨会话并发(每会话一个 worker 队列);
//   - event_id 幂等去重(平台会重试投递);
//   - 挂起中的 ask_user / 审批问题优先截获该会话的下一条消息。
type Dispatcher struct {
	logger *slog.Logger

	mu      sync.Mutex
	workers map[string]chan job     // 会话 key → 串行队列
	pending map[string]chan string  // 会话 key → 等待用户回复的通道
	seen    map[string]struct{}     // 事件去重
	seenQ   []string                // 去重集的淘汰队列
}

type job struct {
	b  Binding
	in Inbound
}

// NewDispatcher 创建分发器。
func NewDispatcher(logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		logger:  logger,
		workers: map[string]chan job{},
		pending: map[string]chan string{},
		seen:    map[string]struct{}{},
	}
}

// Handler 返回绑定到 b 的 InboundHandler,交给 Channel.Start。
func (d *Dispatcher) Handler(b Binding) InboundHandler {
	if b.SessionMapping == "" {
		b.SessionMapping = "chat"
	}
	if b.AskTimeout <= 0 {
		b.AskTimeout = 10 * time.Minute
	}
	return func(ctx context.Context, in Inbound) {
		if in.EventID != "" && d.duplicate(in.EventID) {
			return
		}
		key := d.sessionKey(b, in.Conv)

		// 有挂起的提问:这条消息是答案,不开新一轮。
		d.mu.Lock()
		if ch, ok := d.pending[key]; ok {
			delete(d.pending, key)
			d.mu.Unlock()
			select {
			case ch <- in.Text:
			default:
			}
			return
		}
		q := d.workers[key]
		if q == nil {
			q = make(chan job, 16)
			d.workers[key] = q
			go d.work(key, q)
		}
		d.mu.Unlock()

		select {
		case q <- job{b: b, in: in}:
		default:
			_, _ = b.Channel.Send(ctx, in.Conv, Outbound{Text: "消息太多啦,请稍后再试。"})
		}
	}
}

func (d *Dispatcher) work(key string, q chan job) {
	for j := range q {
		d.run(key, j)
	}
}

func (d *Dispatcher) run(key string, j job) {
	// IM 消息处理与单次请求生命周期解耦,用独立的长超时上下文。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// HITL 桥接:ask_user 与审批走这个 IM 会话。
	ctx = runctx.WithInteractor(ctx, &imInteractor{d: d, b: j.b, conv: j.in.Conv, key: key})

	if j.b.ReplyMode == "stream" {
		if err := d.streamReply(ctx, j); err == nil {
			return
		}
		// 流式失败(如通道不支持 Update)退化为整段回复。
	}
	answer, err := j.b.Agent.Run(ctx, key, j.in.Text)
	if err != nil {
		d.logger.Error("agent run failed", slog.String("session", key), slog.String("err", err.Error()))
		answer = "处理失败:" + err.Error()
	}
	if _, err := j.b.Channel.Send(ctx, j.in.Conv, Outbound{Text: answer, Markdown: true}); err != nil {
		d.logger.Error("send reply failed", slog.String("session", key), slog.String("err", err.Error()))
	}
}

// streamReply 先发占位消息,拿流式增量按节流间隔刷新同一条消息。
func (d *Dispatcher) streamReply(ctx context.Context, j job) error {
	msgID, err := j.b.Channel.Send(ctx, j.in.Conv, Outbound{Text: "思考中...", Markdown: true})
	if err != nil {
		return err
	}
	sr, err := j.b.Agent.Stream(ctx, d.sessionKey(j.b, j.in.Conv), j.in.Text)
	if err != nil {
		return err
	}
	defer sr.Close()

	var sb strings.Builder
	lastFlush := time.Now()
	flush := func() error {
		return j.b.Channel.Update(ctx, j.in.Conv, msgID, Outbound{Text: sb.String(), Markdown: true})
	}
	for {
		chunk, e := sr.Recv()
		if e != nil {
			break
		}
		sb.WriteString(chunk.Content)
		if time.Since(lastFlush) > time.Second {
			if err := flush(); err != nil {
				return err
			}
			lastFlush = time.Now()
		}
	}
	if sb.Len() == 0 {
		sb.WriteString("(无输出)")
	}
	return flush()
}

func (d *Dispatcher) sessionKey(b Binding, conv ConvRef) string {
	key := fmt.Sprintf("%s-%s", conv.Channel, conv.Chat)
	if b.SessionMapping == "chat_user" {
		key += "-" + conv.User
	}
	return key
}

func (d *Dispatcher) duplicate(eventID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[eventID]; ok {
		return true
	}
	d.seen[eventID] = struct{}{}
	d.seenQ = append(d.seenQ, eventID)
	if len(d.seenQ) > 4096 {
		delete(d.seen, d.seenQ[0])
		d.seenQ = d.seenQ[1:]
	}
	return false
}

// imInteractor 把 IM 会话桥接为 runctx.Interactor:
// 提问发到会话里,该会话的下一条用户消息即答案。进程内挂起等待;
// 跨进程重启的持久化挂起是后续 roadmap。
type imInteractor struct {
	d    *Dispatcher
	b    Binding
	conv ConvRef
	key  string
}

func (i *imInteractor) Ask(ctx context.Context, question string) (string, error) {
	return i.await(ctx, question)
}

func (i *imInteractor) Approve(ctx context.Context, req runctx.ApprovalRequest) (bool, error) {
	q := fmt.Sprintf("需要你批准一个操作:\n%s\n参数:%s\n回复「同意」执行,回复其他内容取消。", req.Description, req.Arguments)
	ans, err := i.await(ctx, q)
	if err != nil {
		return false, err
	}
	ans = strings.TrimSpace(ans)
	return ans == "同意" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes"), nil
}

func (i *imInteractor) await(ctx context.Context, question string) (string, error) {
	ch := make(chan string, 1)
	i.d.mu.Lock()
	i.d.pending[i.key] = ch
	i.d.mu.Unlock()
	defer func() {
		i.d.mu.Lock()
		delete(i.d.pending, i.key)
		i.d.mu.Unlock()
	}()

	if _, err := i.b.Channel.Send(ctx, i.conv, Outbound{Text: question}); err != nil {
		return "", err
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-time.After(i.b.AskTimeout):
		return "", fmt.Errorf("等待用户回复超时")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
