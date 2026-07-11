package serving

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/observe"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// Binding 把一个 channel.Channel 路由到一个 Agent。
type Binding struct {
	Channel channel.Channel
	Agent   Runnable
	// SessionMapping:chat(群共享会话)| chat_user(群内每人独立会话)。
	SessionMapping string
	// ReplyMode 是**无装饰器时的内置默认策略**:text(整段回复,跳过
	// processing)| card(占位卡+原地更新)| stream(占位+流式刷新)。
	// 装了 Decorator 后生命周期全模式统一驱动,发不发由装饰器 Skip 定。
	// card/stream 需通道支持 Update,不支持自动退化为整段。
	ReplyMode string
	// AskTimeout 是 ask_user / 审批等待用户回复的超时,默认 10 分钟。
	AskTimeout time.Duration
	// Placeholder 是 processing 占位文案的快捷覆盖,空 = 取 Texts.Placeholder
	// (再空则英文默认「⏳ Working…」)。
	Placeholder string
	// Texts 覆盖面向用户的文案(nil = 全英文默认);部分填充即可,空字段
	// 回落默认。IM 部署(如中文机器人)在此配中文。
	Texts *Texts
	// Decorator 装饰每条出站消息(nil = 不装饰,按 ReplyMode 默认策略)。
	Decorator Decorator
	// OnProgress 是第三方进度订阅(装了它,内置的卡片过程更新让位)。
	OnProgress ProgressHandler
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
	// origin 承载入站 ctx 的**值**(如第三方 trace 框架写入的 logid)。run
	// 里以 WithoutCancel 剥离其取消/超时后作为根,让 baggage 穿到 agent/
	// decorator;取消由 run 自建的长超时接管。跨进程恢复的轮次此值为空。
	origin context.Context
}

// NewDispatcher 创建分发器,并确保进度事件发射切面已挂载(幂等)。
func NewDispatcher(logger *slog.Logger) *Dispatcher {
	observe.EnsureProgressEvents()
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
		d.mu.Unlock()
		// 挂起模式:会话有持久化的挂起轮次 → 这条消息是答案,
		// 记入交互日志并以原输入重放该轮(进程重启后同样走这里)。
		// 在锁外做:认领是 KV 上的原子操作(TakePendingTurn),不需要
		// dispatcher 锁;redis 抖动时持锁 I/O 会冻结全部会话的分发。
		if d.suspendKV != nil {
			if rec, resumed, err := resumePending(ctx, d.suspendKV, key, in.Text); err != nil {
				// fail-open:挂起后端故障时这条消息按**新输入**继续走,
				// 不能 return——suspendKV 非空时每条消息都过这里,后端一坏
				// 直接返回等于整个机器人失联(实测事故)。挂起记录没被
				// 认领(原子认领失败即原样留存),后端恢复后下一条消息
				// 仍能恢复;代价只是故障窗口内"本想答复挂起问题"的消息
				// 被当成新话题,远好于全量丢弃。
				d.logger.Error("resume pending failed; continuing as new input",
					slog.String("session", key), slog.String("err", err.Error()))
			} else if resumed {
				if !d.enqueue(ctx, b, channel.Inbound{Conv: in.Conv, Text: rec.Input}, rec.TurnID) {
					// 队满回滚:认领成功但没排进队,挂起记录必须放回去,
					// 否则这轮永久丢失(用户答案已记,重发即可再触发)。
					if err := suspendSave(ctx, d.suspendKV, key, rec); err != nil {
						d.logger.Error("rollback pending turn failed", slog.String("session", key), slog.String("err", err.Error()))
					}
				}
				return
			}
		}
		// 运行进行中:控制类消息旁路串行队列,即时生效——
		// "停止"不能排在它要停止的任务后面。文本指令与 HTTP 的
		// /control 端点是同一 Agent.Interrupt/Steer 机制的两个传输入口。
		d.mu.Lock()
		if d.running[key] {
			if isInterruptText(in.Text) {
				d.mu.Unlock()
				b.Agent.Interrupt(key)
				_, _ = d.send(ctx, b, in.Conv, channel.Outbound{Text: b.texts().Stopped})
				return
			}
			if steer, ok := steerText(in.Text); ok {
				d.mu.Unlock()
				b.Agent.Steer(key, steer)
				_, _ = d.send(ctx, b, in.Conv, channel.Outbound{Text: b.texts().Steered})
				return
			}
		}
		d.mu.Unlock()
		d.enqueue(ctx, b, in, "")
	}
}

// enqueue 把一轮任务放进会话的串行队列;队满返回 false(调用方决定
// 是否回滚认领)。turnID 非空表示恢复的轮次。
func (d *Dispatcher) enqueue(ctx context.Context, b Binding, in channel.Inbound, turnID string) bool {
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
	case q <- job{b: b, in: in, turnID: turnID, origin: ctx}:
		return true
	default:
		_, _ = b.Channel.Send(ctx, in.Conv, channel.Outbound{Text: b.texts().Overloaded})
		return false
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

	// IM 消息处理与单次请求生命周期解耦:剥离入站 ctx 的取消/超时(否则
	// ACK 返回即被取消),但**保留其值**——第三方 trace baggage(如 logid)
	// 借此穿到 agent/decorator。取消由独立的长超时接管。跨进程恢复的轮次
	// origin 为空,退化为纯根。
	base := j.origin
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), 30*time.Minute)
	defer cancel()
	// 终端用户身份(IM 的发送者)装入 ctx:长期记忆用户级作用域据此隔离。
	ctx = runctx.WithUser(ctx, j.in.Conv.User)
	// 入口层拦截:第三方可在此把传输 baggage 提升/派生进 ctx,与 HTTP/A2A
	// 统一。此刻 origin 的 baggage 已由 WithoutCancel 保活。
	ctx = applyContextHooks(ctx, InboundInfo{Channel: j.in.Conv.Channel, User: j.in.Conv.User, Session: key})

	// 挂起模式:共享编排(suspendturn.go)装配可挂起交互通道与效果/
	// 交互日志,IM 的传输策略是问句发进会话(带 question 语义过装饰器)。
	// 非挂起模式:进程内阻塞等待的 HITL 桥接(原行为)。
	var turn *suspendTurn
	if d.suspendKV != nil {
		conv := j.in.Conv
		ctx, turn = beginSuspendTurn(ctx, d.suspendKV, j.turnID, func(ctx context.Context, q string) error {
			_, err := d.send(ctx, j.b, conv, channel.Outbound{Kind: channel.KindQuestion, Text: q})
			return err
		})
	} else {
		ctx = runctx.WithInteractor(ctx, &imInteractor{d: d, b: j.b, conv: j.in.Conv, key: key})
	}

	// 无装饰器 + stream 模式(且非挂起):沿用流式刷新路径(内置默认策略)。
	if j.b.Decorator == nil && j.b.ReplyMode == "stream" && d.suspendKV == nil {
		if err := d.streamReply(ctx, j); err == nil {
			return
		}
		// 流式失败(如通道不支持 Update)退化为整段回复。
	}

	// 生命周期(全模式统一):processing 首发 → 过程更新 → 收口。
	// 每一步过装饰器,发不发由装饰器 Skip 定;无装饰器按 ReplyMode
	// 内置默认策略(text 跳过 processing,card 占位+原地更新)。
	lc := newLifecycle(d, j.b, j.in.Conv)
	lc.openProcessing(ctx)

	// 进度订阅:第三方 OnProgress 优先(内置让位);否则装饰器/占位卡
	// 在场时装内置订阅者(过程行节流刷新 + answer 带全量过程)。
	// 发射与 reply_mode 无关——事件总是产生,有订阅才投递。
	if j.b.OnProgress != nil {
		conv := j.in.Conv
		ctx = runctx.WithProgress(ctx, func(c context.Context, ev runctx.ProgressEvent) {
			j.b.OnProgress(c, conv, ev)
		})
	} else if lc.trackProgress() {
		ctx = runctx.WithProgress(ctx, lc.onEvent)
	}

	answer, runErr := j.b.Agent.Run(ctx, key, j.in.Text)
	if turn != nil {
		_, suspended, err := turn.finish(ctx, key, j.in.Text, runErr)
		if suspended {
			// 问句已由 notify 送达;占位收口为等待态,不留悬挂的"处理中"。
			lc.close(ctx, channel.KindQuestion, j.b.texts().Suspended)
			return
		}
		runErr = err // 善终清理已在 finish 内;挂起持久化失败按错误收口
	}
	if runErr != nil {
		d.logger.Error("agent run failed", slog.String("session", key), slog.String("err", runErr.Error()))
		lc.close(ctx, channel.KindError, fmt.Sprintf(j.b.texts().Failure, runErr.Error()))
		return
	}
	lc.close(ctx, channel.KindAnswer, answer)
}

// streamReply 先发占位消息,拿流式增量按节流间隔刷新同一条消息。
func (d *Dispatcher) streamReply(ctx context.Context, j job) error {
	msgID, err := j.b.Channel.Send(ctx, j.in.Conv, channel.Outbound{Text: j.b.texts().Thinking, Markdown: true})
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
	ans, err := i.await(ctx, fmt.Sprintf(i.b.texts().Approval, req.Description, req.Arguments))
	if err != nil {
		return false, err
	}
	return suspend.IsAffirmative(ans), nil
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

	if _, err := i.d.send(ctx, i.b, i.conv, channel.Outbound{Kind: channel.KindQuestion, Text: question}); err != nil {
		return "", err
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-time.After(i.b.AskTimeout):
		return "", fmt.Errorf("%s", i.b.texts().AskTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
