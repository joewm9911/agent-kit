// Package agent 提供 Agent 门面:主循环 Runner + 会话记忆 + 运行时
// 保障(预算、结构化输出)的组合体。
//
// Agent 自身实现 capability.Capability —— 一个 Agent 可以作为工具挂到
// 另一个 Agent 的能力面上,天然支持 sub-agent / supervisor 多智能体拓扑。
package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// Agent 是可对外服务的最终产物。
type Agent struct {
	name        string
	description string
	subSeq      atomic.Int64 // 子 agent 形态的调用序号(执行域按调用隔离)
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
	resultKV    store.KV // 大结果暂存后端(digest),nil = 未配置
	resultTTL   time.Duration

	ctlMu    sync.Mutex
	controls map[string]*loop.ControlState // 会话 → 运行控制

	// turnLocks 是会话轮次锁(分条带):同会话的并发轮串行化,防止
	// append 交错把历史写成两轮穿插(IM 有 dispatcher 串行,HTTP 没有)。
	turnLocks [64]sync.Mutex

	// 滚动摘要在途去重与测试同步。
	compactMu  sync.Mutex
	compacting map[string]bool
	compactWG  sync.WaitGroup
}

// turnLock 按会话哈希取条带锁。
func (a *Agent) turnLock(sessionID string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	return &a.turnLocks[h.Sum32()%uint32(len(a.turnLocks))]
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
	// ResultKV 是大结果暂存(digest)的后端与保留时长,由装配层注入;
	// nil = 该 agent 未配置结果暂存(digest 退化为纯截断)。
	ResultKV  store.KV
	ResultTTL time.Duration
}

// New 组装一个 Agent。model 用于结构化输出修复与滚动摘要等门面级调用。
func New(name, description string, runner engine.Runner, m model.ToolCallingChatModel, opts Options) *Agent {
	return &Agent{
		name: name, description: description, runner: runner, model: m,
		store: opts.Store, window: opts.Window, compaction: opts.Compaction,
		structured: opts.Structured, interactor: opts.Interactor,
		approval: opts.Approval, budget: opts.Budget, record: opts.RecordTools,
		resultKV: opts.ResultKV, resultTTL: opts.ResultTTL,
		controls:   map[string]*loop.ControlState{},
		compacting: map[string]bool{},
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
// 同会话的并发轮被串行化;滚动摘要异步执行,不阻塞本轮返回。
func (a *Agent) Run(ctx context.Context, sessionID, input string) (string, error) {
	lock := a.turnLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	ctx = runctx.WithLoopInput(ctx, input) // set-once:loop 原始输入,{$user_input}
	ctx, rec := a.withRecorder(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs := a.controlFor(sessionID)
	cs.BeginTurn(cancel)
	defer cs.EndTurn()
	ctx = loop.WithControl(ctx, cs)

	// 一轮只做一次全量读:织入视图、L4 窗外召回、快照共享同一份历史。
	all, msgs, err := a.loadTurn(ctx, sessionID, input)
	if err != nil {
		return "", err
	}
	ctx = loop.WithTurnHistory(ctx, all)
	// 本轮上下文卫生:结果暂存(digest 的取回来源)与对话快照(fork 的
	// 背景来源)随 ctx 下发,skill/component 内部同样可见。
	ctx = loop.WithResultStore(ctx, loop.NewResultStore(a.resultKV, a.resultTTL))
	ctx = loop.WithConversationSnapshot(ctx, msgs)
	out, err := a.runner.Generate(ctx, msgs)
	if err != nil {
		if cs.Interrupted() {
			// 用户主动叫停:以正常回答收束,轮次照常入会话。
			answer := "已按你的要求中断当前任务。中断前的执行情况见记录,需要时告诉我从哪里继续。"
			if a.store != nil {
				if aerr := a.store.Append(ctx, sessionID, a.turnMessages(rec, input, answer)...); aerr != nil {
					slog.Warn("agent: append interrupted-turn record", "agent", a.name, "session", sessionID, "err", aerr)
				}
			}
			return answer, nil
		}
		// 失败轮落痕:下一轮模型知道上次试到哪、错在哪,重试不再从零摸索;
		// 已执行的工具在执行记录里,避免重复副作用。尽力而为,不掩盖原错误。
		if a.store != nil {
			turn := []*schema.Message{schema.UserMessage(input)}
			if rec != nil {
				if tm := loop.TrajectoryMessage(rec.Records(), a.record); tm != nil {
					turn = append(turn, tm)
				}
			}
			turn = append(turn, schema.SystemMessage(fmt.Sprintf(
				"[上一轮执行失败] 错误:%v。已执行的工具见执行记录,重试时避免重复有副作用的操作。", err)))
			if aerr := a.store.Append(ctx, sessionID, turn...); aerr != nil {
				slog.Warn("agent: append failed-turn record", "agent", a.name, "session", sessionID, "err", aerr)
			}
		}
		return "", err
	}
	answer := out.Content
	if a.structured != nil {
		enforced, eerr := a.structured.Enforce(ctx, a.model, answer)
		if eerr != nil {
			// 与失败轮落痕政策一致:本轮工作(原始回答+执行记录)进会话,
			// 下一轮可基于它修复格式,而不是整轮从零重来。
			if a.store != nil {
				turn := a.turnMessages(rec, input, answer)
				turn = append(turn, schema.SystemMessage(fmt.Sprintf(
					"[结构化输出失败] 上面的回答未能通过 schema 校验:%v。重试时修复格式即可,不要重做已完成的工作。", eerr)))
				if aerr := a.store.Append(ctx, sessionID, turn...); aerr != nil {
					slog.Warn("agent: append structured-failure record", "agent", a.name, "session", sessionID, "err", aerr)
				}
			}
			return "", eerr
		}
		answer = enforced
	}
	if a.store != nil {
		if err := a.store.Append(ctx, sessionID, a.turnMessages(rec, input, answer)...); err != nil {
			return "", fmt.Errorf("append session: %w", err)
		}
		a.scheduleCompact(ctx, sessionID) // 异步:摘要是维护工作,不让用户等
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
//
// 评审栈差异:Run 路径的终答评审(重复终止/收口守卫/拒绝核对/todo
// 收口)在流式下不生效——token 已经发给用户,弹回重答不可能。对终答
// 质量有硬要求的场景用 Run;流式是体验优先的通道。
func (a *Agent) Stream(ctx context.Context, sessionID, input string) (*schema.StreamReader[*schema.Message], error) {
	lock := a.turnLock(sessionID)
	lock.Lock()
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()

	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	ctx = runctx.WithLoopInput(ctx, input) // set-once:loop 原始输入,{$user_input}
	ctx, rec := a.withRecorder(ctx)
	cs := a.controlFor(sessionID)
	cs.BeginTurn(nil) // 流式:中断经调用方断开 ctx,这里只挂插话通道
	ctx = loop.WithControl(ctx, cs)
	all, msgs, err := a.loadTurn(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}
	ctx = loop.WithTurnHistory(ctx, all)
	ctx = loop.WithResultStore(ctx, loop.NewResultStore(a.resultKV, a.resultTTL))
	ctx = loop.WithConversationSnapshot(ctx, msgs)
	sr, err := a.runner.Stream(ctx, msgs)
	if err != nil {
		return nil, err
	}
	if a.store == nil {
		return sr, nil
	}

	// 轮次锁交接给聚合协程:流耗尽、历史落盘后才放行下一轮。
	// 落盘 ctx 剥离取消:HTTP 调用方把流读完就返回、请求 ctx 随即取消,
	// 聚合协程还没写完 redis——整轮历史(输入+轨迹+回答)静默丢失,下一轮
	// 失忆(与 scheduleCompact 的 WithoutCancel 同一理由)。
	locked = false
	bg := context.WithoutCancel(ctx)
	copies := sr.Copy(2)
	go func() {
		defer lock.Unlock()
		defer copies[1].Close()
		var chunks []*schema.Message
		var recvErr error
		for {
			chunk, e := copies[1].Recv()
			if e != nil {
				if e != io.EOF {
					recvErr = e // 流中断 ≠ 正常收尾:部分回答不能冒充完整轮
				}
				break
			}
			chunks = append(chunks, chunk)
		}
		if len(chunks) == 0 {
			return
		}
		full, e := schema.ConcatMessages(chunks)
		if e != nil {
			slog.Warn("agent: concat stream for session append", "agent", a.name, "session", sessionID, "err", e)
			return
		}
		// 流结束即主循环结束,记录器此时已收齐本轮全部工具调用。
		turn := a.turnMessages(rec, input, full.Content)
		turn[len(turn)-1] = full
		if recvErr != nil {
			// 与 Run 的失败落痕同规:截断的回答带上失败标注入史,
			// 下一轮模型知道这是半截,不在其上盖楼。
			turn = append(turn, schema.SystemMessage(fmt.Sprintf(
				"[上一轮流式输出中断] 错误:%v。以上回答可能不完整。", recvErr)))
		}
		if err := a.store.Append(bg, sessionID, turn...); err != nil {
			slog.Warn("agent: append streamed turn", "agent", a.name, "session", sessionID, "err", err)
			return
		}
		a.scheduleCompact(bg, sessionID)
	}()
	return copies[0], nil
}

func (a *Agent) prepare(ctx context.Context, sessionID string) context.Context {
	ctx = runctx.With(ctx, a.name, sessionID)
	ctx = runctx.WithTurnState(ctx) // 轮内状态袋(todo 收口、催办去重等)
	if a.interactor != nil && runctx.GetInteractor(ctx) == nil {
		ctx = runctx.WithInteractor(ctx, a.interactor)
	}
	// Ring 0 策略随 ctx 下发:主循环与 skill/component 内部统一生效。
	ctx = loop.WithApprovalState(ctx, a.approval)
	ctx = loop.WithBudget(ctx, a.budget)
	return ctx
}

// loadTurn 一次性加载本轮所需的全部历史:返回 (全量原始记录, 织入
// 消息)。全量记录经 ctx 共享给 L4 窗外召回,不再重复读 store。
func (a *Agent) loadTurn(ctx context.Context, sessionID, input string) (all, msgs []*schema.Message, err error) {
	if a.store != nil {
		if fl, ok := a.store.(session.FullLoader); ok {
			if all, err = fl.LoadAll(ctx, sessionID); err != nil {
				return nil, nil, fmt.Errorf("load session: %w", err)
			}
			_, view, synthetic := splitSummaryView(all)
			msgs = windowKeepingHead(view, synthetic, a.window)
		} else {
			if msgs, err = a.store.Load(ctx, sessionID); err != nil {
				return nil, nil, fmt.Errorf("load session: %w", err)
			}
		}
	}
	return all, append(msgs, schema.UserMessage(input)), nil
}

// scheduleCompact 异步触发滚动摘要:摘要是后台维护工作(一次模型
// 调用),不让用户等;同会话在途去重,错过的增长下一轮再压(幂等)。
func (a *Agent) scheduleCompact(ctx context.Context, sessionID string) {
	if !a.compaction.Enabled() || a.model == nil || a.store == nil {
		return
	}
	a.compactMu.Lock()
	if a.compacting[sessionID] {
		a.compactMu.Unlock()
		return
	}
	a.compacting[sessionID] = true
	a.compactMu.Unlock()

	a.compactWG.Add(1)
	bg := context.WithoutCancel(ctx) // 轮次 ctx 随返回取消,摘要用无取消副本
	go func() {
		defer a.compactWG.Done()
		defer func() {
			a.compactMu.Lock()
			delete(a.compacting, sessionID)
			a.compactMu.Unlock()
		}()
		a.compact(bg, sessionID)
	}()
}

// WaitCompactions 等待在途滚动摘要完成(优雅关停与测试用)。
func (a *Agent) WaitCompactions() { a.compactWG.Wait() }

// compact 做滚动摘要持久化:视图超过压缩阈值时,把早期部分(含旧
// 摘要)归并为新摘要追加进 store。原始消息不删除,file 后端保留全量
// 记录可审计;后续织入只按最新摘要重建视图。
// 摘要失败静默跳过(压缩是优化,不是正确性前提)。
func (a *Agent) compact(ctx context.Context, sessionID string) {
	fl, ok := a.store.(session.FullLoader)
	if !ok {
		return
	}
	all, err := fl.LoadAll(ctx, sessionID)
	if err != nil {
		return
	}
	covered, view, synthetic := splitSummaryView(all)
	if !a.compaction.Over(view) {
		return
	}
	keep := a.compaction.Keep()
	cut := loop.SafeCut(view, len(view)-keep)
	if cut <= synthetic || cut >= len(view) {
		return
	}
	summary, err := loop.Summarize(ctx, a.model, a.compaction, view[:cut])
	if err != nil {
		return
	}
	// 新摘要覆盖的原始消息数:切割前缀里除去合成注入的部分(摘要+锚定)。
	delta := cut - synthetic
	if err := a.store.Append(ctx, sessionID, makeSummaryMsg(covered+delta, summary)); err != nil {
		slog.Warn("agent: append rolling summary", "agent", a.name, "session", sessionID, "err", err)
		return
	}
	slog.Info("session compacted", "agent", a.name, "session", sessionID,
		"covered", covered+delta, "kept", len(view)-cut, "summary_runes", len([]rune(summary)))
}

// sessionView 按最新滚动摘要重建织入视图。
func sessionView(all []*schema.Message) []*schema.Message {
	_, view, _ := splitSummaryView(all)
	return view
}

// windowKeepingHead 把视图裁剪到最近 window 条**原始消息**,但始终保留
// 合成头部(滚动摘要 + 锚定的首条用户消息)。摘要是老上下文的压缩,不能
// 因窗口裁剪从头部被裁掉——于是 window 与 keep_recent 相互独立,无隐性
// 大小约束:window 只约束保留的近期原文条数,摘要恒在。
func windowKeepingHead(view []*schema.Message, synthetic, window int) []*schema.Message {
	if window <= 0 || len(view)-synthetic <= window {
		return view
	}
	out := make([]*schema.Message, 0, synthetic+window)
	out = append(out, view[:synthetic]...)
	return append(out, view[len(view)-window:]...)
}

const summaryTagPrefix = "[[rolling-summary:"

func makeSummaryMsg(covered int, text string) *schema.Message {
	return schema.SystemMessage(fmt.Sprintf("%s%d]]\n[Conversation summary]\n%s", summaryTagPrefix, covered, text))
}

// splitSummaryView 解析全量历史:剔除所有摘要标记消息,按最新一条
// 摘要重建织入视图,并做锚定保护:
//
//	视图 = [摘要(合成 system,以 [已有摘要] 标注供归并指令识别)]
//	     + [会话首条用户消息原文(锚定:最初的任务不随归并漂移)]
//	     + 摘要覆盖之后的原始消息
//
// covered 为最新摘要已覆盖的原始消息数;synthetic 为视图头部合成
// 注入的消息数(摘要+锚定),压缩切割时用于换算真实覆盖量。
func splitSummaryView(all []*schema.Message) (covered int, view []*schema.Message, synthetic int) {
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
		// 存储格式带 [Conversation summary] 标签;视图侧换成归并指令识别的 [Existing summary]。
		body := strings.TrimPrefix(lastText, "[Conversation summary]\n")
		view = append(view, schema.SystemMessage("[Existing summary]\n"+body))
		synthetic = 1
		// 锚定:最初的任务描述已被摘要覆盖时,原文常驻视图头部——
		// 多次归并后"最初到底要做什么"不漂移。
		for _, m := range raw[:covered] {
			if m.Role == schema.User && m.Content != "" {
				view = append(view, m)
				synthetic = 2
				break
			}
		}
	}
	return covered, append(view, raw[covered:]...), synthetic
}

// RawHistory 返回剔除摘要标记后的原始历史(供相关性召回等使用),
// 以及当前视图未包含的早期部分长度。
func RawHistory(all []*schema.Message) (raw []*schema.Message, beyondView int) {
	covered, _, _ := splitSummaryView(all)
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
		Ref:         capability.Ref{Kind: "agent", Domain: "agents", Name: a.name},
		Description: a.description,
		Params:      capability.SingleParam("task", "交给该 agent 的完整任务描述"),
		Risk:        capability.RiskReadonly, // 子 agent 内部工具各带风险闸门,入口本身无直接副作用
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
	ctx = runctx.WithScopePush(ctx, fmt.Sprintf("sub:%s#%d", a.name, a.subSeq.Add(1)))
	task := capability.ParseSingle(argsJSON, "task")
	out, err := a.runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
	if err != nil {
		return "", err
	}
	return out.Content, nil
}
