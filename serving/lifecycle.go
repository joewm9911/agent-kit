// lifecycle.go:一轮 IM 回复的生命周期驱动
// (设计见 docs/channel-card-design.md §3.1/§4)。
//
// 生命周期全模式统一:processing 首发 → 过程更新 → answer/question/
// error 收口,每一步构造语义 Outbound 送进装饰器,发不发由装饰器
// Skip 决定;无装饰器时按 ReplyMode 内置默认策略(text 跳过
// processing、card 占位+更新)。占位被 Skip/发送失败则后续自动
// Send 而非 Update——机械处理,装饰器不用关心。
package serving

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
)

// progressFlushEvery 是内置订阅者的过程更新节流间隔(IM PATCH 有频控)。
const progressFlushEvery = 2 * time.Second

// send 统一出站入口:装饰 → Skip 判定 → Channel.Send。
// answer/question 被 Skip 记 warn(终稿被吞而用户无感知是事故形态,留证)。
func (d *Dispatcher) send(ctx context.Context, b Binding, conv channel.ConvRef, out channel.Outbound) (string, error) {
	out = decorate(ctx, b, conv, out)
	if out.Skip {
		d.warnSkip(out.Kind)
		return "", nil
	}
	return b.Channel.Send(ctx, conv, out)
}

// update 统一更新入口:装饰 → Skip 判定 → Channel.Update。
func (d *Dispatcher) update(ctx context.Context, b Binding, conv channel.ConvRef, msgID string, out channel.Outbound) error {
	out = decorate(ctx, b, conv, out)
	if out.Skip {
		d.warnSkip(out.Kind)
		return nil
	}
	return b.Channel.Update(ctx, conv, msgID, out)
}

func (d *Dispatcher) warnSkip(kind string) {
	if kind == channel.KindAnswer || kind == channel.KindQuestion {
		d.logger.Warn("decorator skipped critical message", slog.String("kind", kind))
	}
}

// lifecycle 驱动一轮回复:占位、过程行累积与节流刷新、收口。
type lifecycle struct {
	d     *Dispatcher
	b     Binding
	conv  channel.ConvRef
	start time.Time

	mu        sync.Mutex
	msgID     string   // 占位消息 ID("" = 未发/被 Skip → 收口走 Send)
	done      []string // 已完成步骤的过程行(只增不减)
	inflight  string   // 进行中步骤的过程行(随事件替换)
	tools     int      // 工具完成计数(Meta 用)
	lastFlush time.Time
	closed    bool // 收口后停止过程更新
}

func newLifecycle(d *Dispatcher, b Binding, conv channel.ConvRef) *lifecycle {
	return &lifecycle{d: d, b: b, conv: conv, start: time.Now()}
}

func (lc *lifecycle) placeholderText() string {
	if lc.b.Placeholder != "" {
		return lc.b.Placeholder
	}
	return lc.b.texts().Placeholder
}

// openProcessing 发 processing 占位。装饰器在场时全模式统一发起
// (Skip 与否是装饰器的决定);无装饰器只有 card 模式发(内置默认策略)。
func (lc *lifecycle) openProcessing(ctx context.Context) {
	if lc.b.Decorator == nil && lc.b.ReplyMode != "card" {
		return
	}
	id, err := lc.d.send(ctx, lc.b, lc.conv, channel.Outbound{
		Kind: channel.KindProcessing, Text: lc.placeholderText(), Markdown: true,
	})
	if err != nil {
		return // 占位失败不阻断处理,收口退化为 Send
	}
	lc.mu.Lock()
	lc.msgID = id
	lc.mu.Unlock()
}

// trackProgress 判断是否需要安装内置进度订阅:有装饰器(answer 要带
// 全量过程行)或占位已发出(过程更新有落点)。
func (lc *lifecycle) trackProgress() bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.b.Decorator != nil || lc.msgID != ""
}

// onEvent 是内置订阅者:主循环的用户配置能力步骤转过程行,节流刷新
// 占位卡。运行在进度投递 worker 里(异步),Update 网络耗时不影响主流程。
// 过滤策略:只呈现 Scope 为空(主循环)、ScopeKind=custom(用户配置的
// 能力,builtin 的 todo/ask_user 不刷屏)、非模型步骤;component 内部
// 与全量原始事件用 OnProgress 订阅。
func (lc *lifecycle) onEvent(ctx context.Context, ev runctx.ProgressEvent) {
	if ev.Scope != "" || ev.ScopeKind != runctx.ScopeCustom || ev.CapKind == "model" {
		return
	}
	lc.mu.Lock()
	if lc.closed {
		lc.mu.Unlock()
		return
	}
	txt := lc.b.texts()
	switch ev.Status {
	case "start":
		lc.inflight = fmt.Sprintf(txt.StepRunning, ev.Name)
	case "done":
		lc.inflight = ""
		lc.tools++
		lc.done = append(lc.done, fmt.Sprintf(txt.StepDone, ev.Name, ev.Dur.Seconds()))
	case "error":
		lc.inflight = ""
		lc.done = append(lc.done, fmt.Sprintf(txt.StepFailed, ev.Name, ev.Dur.Seconds()))
	}
	flush := lc.msgID != "" && time.Since(lc.lastFlush) >= progressFlushEvery
	if flush {
		lc.lastFlush = time.Now()
	}
	msgID, lines := lc.msgID, lc.snapshotLocked()
	lc.mu.Unlock()

	if flush {
		_ = lc.d.update(ctx, lc.b, lc.conv, msgID, channel.Outbound{
			Kind: channel.KindProcessing, Text: lc.placeholderText(),
			Markdown: true, Progress: lines,
		})
	}
}

func (lc *lifecycle) snapshotLocked() []string {
	lines := append([]string(nil), lc.done...)
	if lc.inflight != "" {
		lines = append(lines, lc.inflight)
	}
	return lines
}

// closeAnswer 以终答收口并携带交付物事实位(装饰器可据此内联定制)。
func (lc *lifecycle) closeAnswer(ctx context.Context, text string, dels []runctx.Deliverable) {
	lc.closeOut(ctx, channel.KindAnswer, text, dels)
}

// close 收口:answer/question/error 统一出口。占位在场先 Update,
// 失败(通道不支持等)退化 Send;占位不在场直接 Send。
func (lc *lifecycle) close(ctx context.Context, kind, text string) {
	lc.closeOut(ctx, kind, text, nil)
}

func (lc *lifecycle) closeOut(ctx context.Context, kind, text string, dels []runctx.Deliverable) {
	lc.mu.Lock()
	lc.closed = true
	msgID, lines, tools := lc.msgID, lc.snapshotLocked(), lc.tools
	lc.mu.Unlock()

	out := channel.Outbound{Kind: kind, Text: text, Markdown: true, Progress: lines, Deliverables: dels}
	if kind == channel.KindAnswer || kind == channel.KindError {
		out.Meta = fmt.Sprintf(lc.b.texts().Summary, time.Since(lc.start).Seconds(), tools)
	}
	if msgID != "" {
		if err := lc.d.update(ctx, lc.b, lc.conv, msgID, out); err == nil {
			return
		}
		// 更新失败退化为整段回复,占位保持原样。
	}
	if kind == channel.KindQuestion && msgID == "" {
		return // 挂起收口只在有占位时才有意义(问句本身是独立消息)
	}
	if _, err := lc.d.send(ctx, lc.b, lc.conv, out); err != nil {
		lc.d.logger.Error("send reply failed", slog.String("kind", kind), slog.String("err", err.Error()))
	}
}
