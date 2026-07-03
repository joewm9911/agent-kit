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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/cloverzhang/agent-kit/capability"
	"github.com/cloverzhang/agent-kit/engine"
	"github.com/cloverzhang/agent-kit/loop"
	"github.com/cloverzhang/agent-kit/runctx"
	"github.com/cloverzhang/agent-kit/session"
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
}

// Options 是 New 的可选项。
type Options struct {
	Store      session.Store
	Window     int
	// Compaction 启用后做滚动摘要持久化:历史超阈值时把早期部分
	// 摘要成一条带 covered 标记的记录追加进 store(不删原始消息,
	// file 后端保留全量可审计);后续织入时视图 = 最新摘要 + 其后消息。
	Compaction loop.CompactionConfig
	Structured *loop.StructuredEnforcer
	Interactor runctx.Interactor
}

// New 组装一个 Agent。model 用于结构化输出修复与滚动摘要等门面级调用。
func New(name, description string, runner engine.Runner, m model.ToolCallingChatModel, opts Options) *Agent {
	return &Agent{
		name: name, description: description, runner: runner, model: m,
		store: opts.Store, window: opts.Window, compaction: opts.Compaction,
		structured: opts.Structured, interactor: opts.Interactor,
	}
}

// Name 返回 agent 名。
func (a *Agent) Name() string { return a.name }

// Description 返回 agent 描述。
func (a *Agent) Description() string { return a.description }

// Run 执行一轮对话:注入运行上下文 → 加载会话历史 → 运行主循环 →
// (可选)结构化输出校验 → 回写历史。
func (a *Agent) Run(ctx context.Context, sessionID, input string) (string, error) {
	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	msgs, err := a.history(ctx, sessionID, input)
	if err != nil {
		return "", err
	}
	out, err := a.runner.Generate(ctx, msgs)
	if err != nil {
		return "", err
	}
	answer := out.Content
	if a.structured != nil {
		if answer, err = a.structured.Enforce(ctx, a.model, answer); err != nil {
			return "", err
		}
	}
	if a.store != nil {
		if err := a.store.Append(ctx, sessionID, schema.UserMessage(input), schema.AssistantMessage(answer, nil)); err != nil {
			return "", fmt.Errorf("append session: %w", err)
		}
		a.maybeCompact(ctx, sessionID)
	}
	return answer, nil
}

// Stream 流式执行一轮对话。返回的流复制两份:一份给调用方,
// 一份在后台聚合后回写会话历史。结构化输出与流式互斥(用 Run)。
func (a *Agent) Stream(ctx context.Context, sessionID, input string) (*schema.StreamReader[*schema.Message], error) {
	ctx = runctx.WithInput(a.prepare(ctx, sessionID), input)
	msgs, err := a.history(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}
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
		_ = a.store.Append(ctx, sessionID, schema.UserMessage(input), full)
	}()
	return copies[0], nil
}

func (a *Agent) prepare(ctx context.Context, sessionID string) context.Context {
	ctx = runctx.With(ctx, a.name, sessionID)
	if a.interactor != nil && runctx.GetInteractor(ctx) == nil {
		ctx = runctx.WithInteractor(ctx, a.interactor)
	}
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
// 内部过程不回流宿主上下文,只返回最终结果。
func (a *Agent) invokeAsSub(ctx context.Context, argsJSON string) (string, error) {
	task := capability.ParseSingle(argsJSON, "task")
	out, err := a.runner.Generate(ctx, []*schema.Message{schema.UserMessage(task)})
	if err != nil {
		return "", err
	}
	return out.Content, nil
}
