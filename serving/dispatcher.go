package serving

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// Binding 把一个 channel.Channel 路由到一个 Agent。
type Binding struct {
	Channel channel.Channel
	Agent   Runnable
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
	workers map[string]chan job    // 会话 key → 串行队列
	pending map[string]chan string // 会话 key → 等待用户回复的通道
	running map[string]bool        // 会话 key → 是否有运行进行中
	seen    map[string]struct{}    // 事件去重
	seenQ   []string               // 去重集的淘汰队列

	// suspendKV 非 nil 时启用挂起模式:ask_user/审批不再阻塞
	// goroutine 等待,而是持久化挂起、答案到达(可跨进程重启)后重放。
	// 后端是注入的 store.KV(file/redis/...),跨进程恢复要求后端持久。
	suspendKV store.KV
}

type job struct {
	b      Binding
	in     channel.Inbound
	turnID string // 挂起模式的轮次标识;恢复的轮次沿用首跑的 ID
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
		running: map[string]bool{},
		seen:    map[string]struct{}{},
	}
}

// EnableSuspend 启用挂起模式:交互等待持久化到注入的 KV 后端,跨进程
// 重启可恢复。挂起模式与流式回复不兼容,启用后流式绑定退化为整段回复。
func (d *Dispatcher) EnableSuspend(kv store.KV) {
	d.suspendKV = kv
}

// Handler 返回绑定到 b 的 channel.InboundHandler,交给 channel.Channel.Start。
func (d *Dispatcher) Handler(b Binding) channel.InboundHandler {
	if b.SessionMapping == "" {
		b.SessionMapping = "chat"
	}
	if b.AskTimeout <= 0 {
		b.AskTimeout = 10 * time.Minute
	}
	return func(ctx context.Context, in channel.Inbound) {
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
		// 挂起模式:会话有持久化的挂起轮次 → 这条消息是答案,
		// 记入交互日志并以原输入重放该轮(进程重启后同样走这里)。
		if d.suspendKV != nil {
			if rec, ok, err := suspend.TakePendingTurn(ctx, d.suspendKV, key); err == nil && ok {
				d.mu.Unlock()
				if err := suspend.AnswerPending(ctx, d.suspendKV, rec.WaitingID, in.Text); err != nil {
					d.logger.Error("record answer failed", slog.String("session", key), slog.String("err", err.Error()))
					return
				}
				d.enqueue(ctx, b, channel.Inbound{Conv: in.Conv, Text: rec.Input}, rec.TurnID)
				return
			}
		}
		// 运行进行中:控制类消息旁路串行队列,即时生效——
		// "停止"不能排在它要停止的任务后面。
		if d.running[key] {
			if isInterruptText(in.Text) {
				d.mu.Unlock()
				b.Agent.Interrupt(key)
				_, _ = b.Channel.Send(ctx, in.Conv, channel.Outbound{Text: "好的,正在停止当前任务。"})
				return
			}
			if steer, ok := steerText(in.Text); ok {
				d.mu.Unlock()
				b.Agent.Steer(key, steer)
				_, _ = b.Channel.Send(ctx, in.Conv, channel.Outbound{Text: "已把你的话带给正在运行的任务。"})
				return
			}
		}
		d.mu.Unlock()
		d.enqueue(ctx, b, in, "")
	}
}

// enqueue 把一轮任务放进会话的串行队列。turnID 非空表示恢复的轮次。
func (d *Dispatcher) enqueue(ctx context.Context, b Binding, in channel.Inbound, turnID string) {
	key := d.sessionKey(b, in.Conv)
	d.mu.Lock()
	q := d.workers[key]
	if q == nil {
		q = make(chan job, 16)
		d.workers[key] = q
		go d.work(key, q)
	}
	d.mu.Unlock()

	select {
	case q <- job{b: b, in: in, turnID: turnID}:
	default:
		_, _ = b.Channel.Send(ctx, in.Conv, channel.Outbound{Text: "消息太多啦,请稍后再试。"})
	}
}

func (d *Dispatcher) work(key string, q chan job) {
	for j := range q {
		d.run(key, j)
	}
}

func (d *Dispatcher) run(key string, j job) {
	d.mu.Lock()
	d.running[key] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.running, key)
		d.mu.Unlock()
	}()

	// IM 消息处理与单次请求生命周期解耦,用独立的长超时上下文。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// 终端用户身份(IM 的发送者)装入 ctx:长期记忆用户级作用域据此隔离。
	ctx = runctx.WithUser(ctx, j.in.Conv.User)

	// 挂起模式:可挂起的交互通道 + 效果/交互日志随 ctx 下发。
	// 非挂起模式:进程内阻塞等待的 HITL 桥接(原行为)。
	var journal *suspend.Journal
	if d.suspendKV != nil {
		turnID := j.turnID
		if turnID == "" {
			turnID = suspend.NewTurnID()
		}
		journal = suspend.NewJournal(d.suspendKV, turnID)
		ctx = suspend.WithJournal(ctx, journal)
		conv := j.in.Conv
		ctx = runctx.WithInteractor(ctx, suspend.Interactor(journal, func(ctx context.Context, q string) error {
			_, err := j.b.Channel.Send(ctx, conv, channel.Outbound{Text: q})
			return err
		}))
	} else {
		ctx = runctx.WithInteractor(ctx, &imInteractor{d: d, b: j.b, conv: j.in.Conv, key: key})
	}

	// 挂起模式与流式回复不兼容(挂起需要整轮退栈),退化整段。
	if j.b.ReplyMode == "stream" && d.suspendKV == nil {
		if err := d.streamReply(ctx, j); err == nil {
			return
		}
		// 流式失败(如通道不支持 Update)退化为整段回复。
	}
	answer, err := j.b.Agent.Run(ctx, key, j.in.Text)
	if err != nil {
		var suspended *suspend.ErrSuspended
		if journal != nil && errors.As(err, &suspended) {
			// 问题已送达用户;持久化挂起轮次后整轮退栈,不占任何资源。
			saveErr := suspend.SavePendingTurn(ctx, d.suspendKV, key, suspend.PendingTurn{
				TurnID: journal.TurnID(), Input: j.in.Text, WaitingID: suspended.InteractionID,
			})
			if saveErr != nil {
				d.logger.Error("save pending turn failed", slog.String("session", key), slog.String("err", saveErr.Error()))
			}
			return
		}
		d.logger.Error("agent run failed", slog.String("session", key), slog.String("err", err.Error()))
		answer = "处理失败:" + err.Error()
	} else if journal != nil {
		journal.CompleteTurn(ctx) // 一轮善终,清理该轮日志
	}
	if _, err := j.b.Channel.Send(ctx, j.in.Conv, channel.Outbound{Text: answer, Markdown: true}); err != nil {
		d.logger.Error("send reply failed", slog.String("session", key), slog.String("err", err.Error()))
	}
}

// streamReply 先发占位消息,拿流式增量按节流间隔刷新同一条消息。
func (d *Dispatcher) streamReply(ctx context.Context, j job) error {
	msgID, err := j.b.Channel.Send(ctx, j.in.Conv, channel.Outbound{Text: "思考中...", Markdown: true})
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
		return j.b.Channel.Update(ctx, j.in.Conv, msgID, channel.Outbound{Text: sb.String(), Markdown: true})
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

// isInterruptText 识别叫停指令。
func isInterruptText(s string) bool {
	switch strings.TrimSpace(s) {
	case "停", "停止", "停下", "取消", "别做了", "stop", "/stop", "cancel":
		return true
	}
	return false
}

// steerText 识别插话指令:「插话:」「补充:」前缀的内容注入运行中的任务。
func steerText(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"插话:", "插话：", "补充:", "补充："} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// sessionKey 派生会话 key:话题消息按话题细分(同一群里每个话题是
// 独立会话,话题即上下文);SessionMapping 在话题维度之上再叠加
// 用户隔离(chat_user)。
func (d *Dispatcher) sessionKey(b Binding, conv channel.ConvRef) string {
	key := fmt.Sprintf("%s-%s", conv.Channel, conv.Chat)
	if conv.Thread != "" {
		key += "-" + conv.Thread
	}
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
	conv channel.ConvRef
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

	if _, err := i.b.Channel.Send(ctx, i.conv, channel.Outbound{Text: question}); err != nil {
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
