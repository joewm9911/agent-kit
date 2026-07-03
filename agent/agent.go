// Package agent 提供 Agent 门面:主循环 Runner + 会话记忆 + 运行时
// 保障(预算、结构化输出)的组合体。
//
// Agent 自身实现 capability.Capability —— 一个 Agent 可以作为工具挂到
// 另一个 Agent 的能力面上,天然支持 sub-agent / supervisor 多智能体拓扑。
package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/runctx"
	"github.com/joewm9911/agent-kit/session"
)

// Agent 是可对外服务的最终产物。
type Agent struct {
	name        string
	description string
	runner      engine.Runner
	store       session.Store // nil = 无会话记忆
	window      int           // 织入历史的窗口上限
	compaction  loop.CompactionConfig
	model       model.ToolCallingChatModel
	structured  *loop.StructuredEnforcer // nil = 不启用
	interactor  runctx.Interactor        // 默认交互通道,可被 ctx 覆盖
	approval    *loop.ApprovalState
	budget      *loop.BudgetGate
	record      loop.RecordMode

	ctlMu    sync.Mutex
	controls map[string]*loop.ControlState // 会话 → 运行控制
}

// Options 是 New 的可选项。
type Options struct {
	Store  session.Store
	Window int
	// Compaction 启用后做滚动摘要持久化:历史超阈值时把早期部分
	// 摘要成一条带 covered 标记的记录追加进 store(不删原始消息,
	// file 后端保留全量可审计);后续织入时视图 = 最新摘要 + 其后消息。
	Compaction loop.CompactionConfig
	Structured *loop.StructuredEnforcer
	Interactor runctx.Interactor
	// Approval 与 Budget 在每次运行时装入 ctx,对主循环与
	// skill/component 内部的 Ring 0 闸门统一生效。
	Approval *loop.ApprovalState
	Budget   *loop.BudgetGate
	// RecordTools 控制本轮工具轨迹随会话持久化的详略(默认 off,
	// config 层默认 summary)。
	RecordTools loop.RecordMode
}

// New 组装一个 Agent。model 用于结构化输出修复与滚动摘要等门面级调用。
func New(name, description string, runner engine.Runner, m model.ToolCallingChatModel, opts Options) *Agent {
	return &Agent{
		name: name, description: description, runner: runner, model: m,
		store: opts.Store, window: opts.Window, compaction: opts.Compaction,
		structured: opts.Structured, interactor: opts.Interactor,
		approval: opts.Approval, budget: opts.Budget, record: opts.RecordTools,
		controls: map[string]*loop.ControlState{},
	}
}

// controlFor 取(或创建)会话的运行控制。
func (a *Agent) controlFor(sessionID string) *loop.ControlState {
	a.ctlMu.Lock()
	defer a.ctlMu.Unlock()
	cs, ok := a.controls[sessionID]
	if !ok {
		if len(a.controls) > 4096 { // 粗粒度防泄漏
			a.controls = map[string]*loop.ControlState{}
		}
		cs = &loop.ControlState{}
		a.controls[sessionID] = cs
	}
	return cs
}

// Interrupt 叫停某会话正在进行的运行:下一次工具调用前生效,并取消
// 当前轮 ctx(终止进行中的模型调用与并行分支)。
func (a *Agent) Interrupt(sessionID string) {
	a.controlFor(sessionID).Interrupt()
}

// Steer 向某会话正在进行的运行注入一条用户插话,随下一个工具结果
// 送达模型(中途驾驶,不打断循环)。
func (a *Agent) Steer(sessionID, msg string) {
	a.controlFor(sessionID).Steer(msg)
}

// Name 返回 agent 名。
func (a *Agent) Name() string { return a.name }

// Description 返回 agent 描述。
func (a *Agent) Description() string { return a.description }

// Run 执行一轮对话:注入运行上下文 → 加载会话历史 → 运行主循环 →
// (可选)结构化输出校验 → 回写历史(含本轮工具轨迹)。
func (a *Agent) Run(ctx context.Context, sessionID, input string) (string, error) {
	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	ctx, rec := a.withRecorder(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs := a.controlFor(sessionID)
	cs.BeginTurn(cancel)
	defer cs.EndTurn()
	ctx = loop.WithControl(ctx, cs)

	msgs, err := a.history(ctx, sessionID, input)
	if err != nil {
		return "", err
	}
	// 本轮上下文卫生:结果暂存(digest 的取回来源)与对话快照(fork 的
	// 背景来源)随 ctx 下发,skill/component 内部同样可见。
	ctx = loop.WithResultStore(ctx, loop.NewResultStore())
	ctx = loop.WithConversationSnapshot(ctx, msgs)
	out, err := a.runner.Generate(ctx, msgs)
	if err != nil {
		if cs.Interrupted() {
			// 用户主动叫停:以正常回答收束,轮次照常入会话。
			answer := "已按你的要求中断当前任务。中断前的执行情况见记录,需要时告诉我从哪里继续。"
			if a.store != nil {
				_ = a.store.Append(ctx, sessionID, a.turnMessages(rec, input, answer)...)
			}
			return answer, nil
		}
		return "", err
	}
	answer := out.Content
	if a.structured != nil {
		if answer, err = a.structured.Enforce(ctx, a.model, answer); err != nil {
			return "", err
		}
	}
	if a.store != nil {
		if err := a.store.Append(ctx, sessionID, a.turnMessages(rec, input, answer)...); err != nil {
			return "", fmt.Errorf("append session: %w", err)
		}
		a.maybeCompact(ctx, sessionID)
	}
	return answer, nil
}

// withRecorder 在启用轨迹记录且有会话存储时装入记录器。
func (a *Agent) withRecorder(ctx context.Context) (context.Context, *loop.ToolRecorder) {
	if a.store == nil || a.record == "" || a.record == loop.RecordOff {
		return ctx, nil
	}
	rec := &loop.ToolRecorder{}
	return loop.WithToolRecorder(ctx, rec), rec
}

// turnMessages 组装一轮的持久化消息:user → [执行记录] → assistant。
// 工具轨迹入会话后,下一轮模型知道自己做过什么、看到过什么。
func (a *Agent) turnMessages(rec *loop.ToolRecorder, input, answer string) []*schema.Message {
	msgs := []*schema.Message{schema.UserMessage(input)}
	if rec != nil {
		if tm := loop.TrajectoryMessage(rec.Records(), a.record); tm != nil {
			msgs = append(msgs, tm)
		}
	}
	return append(msgs, schema.AssistantMessage(answer, nil))
}

// Stream 流式执行一轮对话。返回的流复制两份:一份给调用方,
// 一份在后台聚合后回写会话历史(含本轮工具轨迹)。结构化输出与
// 流式互斥(用 Run)。
func (a *Agent) Stream(ctx context.Context, sessionID, input string) (*schema.StreamReader[*schema.Message], error) {
	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	ctx, rec := a.withRecorder(ctx)
	cs := a.controlFor(sessionID)
	cs.BeginTurn(nil) // 流式:中断经调用方断开 ctx,这里只挂插话通道
	ctx = loop.WithControl(ctx, cs)
	msgs, err := a.history(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}
	ctx = loop.WithResultStore(ctx, loop.NewResultStore())
	ctx = loop.WithConversationSnapshot(ctx, msgs)
	sr, err := a.runner.Stream(ctx, msgs)
	if err != nil {
		return nil, err
	}
	if a.store == nil {
		return sr, nil
	}

	copies := sr.Copy(2)
	go func() {
		defer copies[1].Close()
		var chunks []*schema.Message
		for {
			chunk, e := copies[1].Recv()
			if e != nil {
				break
			}
			chunks = append(chunks, chunk)
		}
		if len(chunks) == 0 {
			return
		}
		full, e := schema.ConcatMessages(chunks)
		if e != nil {
			return
		}
		// 流结束即主循环结束,记录器此时已收齐本轮全部工具调用。
		turn := a.turnMessages(rec, input, full.Content)
		turn[len(turn)-1] = full
		_ = a.store.Append(ctx, sessionID, turn...)
	}()
	return copies[0], nil
}

func (a *Agent) prepare(ctx context.Context, sessionID string) context.Context {
	ctx = runctx.With(ctx, a.name, sessionID)
	if a.interactor != nil && runctx.GetInteractor(ctx) == nil {
		ctx = runctx.WithInteractor(ctx, a.interactor)
	}
	// Ring 0 策略随 ctx 下发:主循环与 skill/component 内部统一生效。
	ctx = loop.WithApprovalState(ctx, a.approval)
	ctx = loop.WithBudget(ctx, a.budget)
	return ctx
}

// history 织入会话历史。后端支持全量读取时按滚动摘要重建视图:
// [最新摘要] + 其后的原始消息;否则退化为窗口 Load。
func (a *Agent) history(ctx context.Context, sessionID, input string) ([]*schema.Message, error) {
	var msgs []*schema.Message
	if a.store != nil {
		if fl, ok := a.store.(session.FullLoader); ok {
			all, err := fl.LoadAll(ctx, sessionID)
			if err != nil {
				return nil, fmt.Errorf("load session: %w", err)
			}
			msgs = sessionView(all)
			if a.window > 0 && len(msgs) > a.window {
				msgs = msgs[len(msgs)-a.window:]
			}
		} else {
			h, err := a.store.Load(ctx, sessionID)
			if err != nil {
				return nil, fmt.Errorf("load session: %w", err)
			}
			msgs = h
		}
	}
	return append(msgs, schema.UserMessage(input)), nil
}

// maybeCompact 做滚动摘要持久化:视图超过压缩阈值时,把早期部分
// (含旧摘要)归并为新摘要追加进 store。原始消息不删除,file 后端
// 保留全量记录可审计;后续 history 只按最新摘要重建视图。
// 摘要失败静默跳过(压缩是优化,不是正确性前提)。
func (a *Agent) maybeCompact(ctx context.Context, sessionID string) {
	if !a.compaction.Enabled() || a.model == nil {
		return
	}
	fl, ok := a.store.(session.FullLoader)
	if !ok {
		return
	}
	all, err := fl.LoadAll(ctx, sessionID)
	if err != nil {
		return
	}
	covered, view := splitSummary(all)
	if !a.compaction.Over(view) {
		return
	}
	keep := a.compaction.Keep()
	cut := loop.SafeCut(view, len(view)-keep)
	if cut <= 0 || cut >= len(view) {
		return
	}
	summary, err := loop.Summarize(ctx, a.model, view[:cut])
	if err != nil {
		return
	}
	// 新摘要覆盖的原始消息数:视图前缀里除去合成的旧摘要那一条。
	delta := cut
	if len(view) > 0 && isSummaryMsg(view[0]) {
		delta = cut - 1
	}
	_ = a.store.Append(ctx, sessionID, makeSummaryMsg(covered+delta, summary))
}

// sessionView 按最新滚动摘要重建织入视图。
func sessionView(all []*schema.Message) []*schema.Message {
	_, view := splitSummary(all)
	return view
}

const summaryTagPrefix = "[[rolling-summary:"

func makeSummaryMsg(covered int, text string) *schema.Message {
	return schema.SystemMessage(fmt.Sprintf("%s%d]]\n[会话摘要]\n%s", summaryTagPrefix, covered, text))
}

func isSummaryMsg(m *schema.Message) bool {
	return m.Role == schema.System && strings.HasPrefix(m.Content, "[会话摘要]")
}

// splitSummary 解析全量历史:剔除所有摘要标记消息,按最新一条摘要
// 重建视图 = [摘要(合成 system)] + 其后的原始消息。covered 为最新
// 摘要已覆盖的原始消息数。
func splitSummary(all []*schema.Message) (covered int, view []*schema.Message) {
	var raw []*schema.Message
	var lastText string
	for _, m := range all {
		if m.Role == schema.System && strings.HasPrefix(m.Content, summaryTagPrefix) {
			rest := m.Content[len(summaryTagPrefix):]
			end := strings.Index(rest, "]]")
			if end < 0 {
				continue
			}
			n, err := strconv.Atoi(rest[:end])
			if err != nil {
				continue
			}
			covered = n
			lastText = strings.TrimPrefix(rest[end+2:], "\n")
			continue
		}
		raw = append(raw, m)
	}
	if covered > len(raw) {
		covered = len(raw)
	}
	if lastText != "" {
		view = append(view, schema.SystemMessage(lastText))
	}
	return covered, append(view, raw[covered:]...)
}

// RawHistory 返回剔除摘要标记后的原始历史(供相关性召回等使用),
// 以及当前视图未包含的早期部分长度。
func RawHistory(all []*schema.Message) (raw []*schema.Message, beyondView int) {
	covered, _ := splitSummary(all)
	for _, m := range all {
		if m.Role == schema.System && strings.HasPrefix(m.Content, summaryTagPrefix) {
			continue
		}
		raw = append(raw, m)
	}
	return raw, covered
}

// ---- Agent 即能力:实现 capability.Capability ----

// Meta 实现 capability.Capability。
func (a *Agent) Meta() capability.Meta {
	return capability.Meta{
		Ref:         capability.Ref{Kind: "agent", Provider: "local", Namespace: "agents", Name: a.name},
		Description: a.description,
		Params:      capability.SingleParam("task", "交给该 agent 的完整任务描述"),
	}
}

// AsTool 实现 capability.Capability。
func (a *Agent) AsTool(ctx context.Context) (tool.BaseTool, error) {
	c := capability.New(a.Meta(), a.invokeAsSub)
	return c.AsTool(ctx)
}

// AsLambda 实现 capability.Capability。
func (a *Agent) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(a.invokeAsSub), nil
}

// invokeAsSub 作为子 agent 被调用:独立会话,不与上级会话串历史;
// 内部过程不回流宿主上下文,只返回最终结果。使用点声明 context: fork
// 时,以调用方对话快照 + 任务起步(背景无损继承,隔离方向不变)。
// 压执行域:todo 等按域隔离的运行时状态与宿主分键,互不覆盖
// (预算/审批刻意不分——治理归调用方的会话账本)。
func (a *Agent) invokeAsSub(ctx context.Context, argsJSON string) (string, error) {
	ctx = runctx.WithScopePush(ctx, "sub:"+a.name)
	task := capability.ParseSingle(argsJSON, "task")
	out, err := a.runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
	if err != nil {
		return "", err
	}
	return out.Content, nil
}
