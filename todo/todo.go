// Package todo 提供内置的计划外化能力(todo_write/todo_read)。
//
// todo 的纪律是 harness 强制的,不靠模型自觉,三道保证:
//   - 写入校验:状态枚举、最多一个 in_progress、内容规模,违规拒绝并纠正;
//   - 每轮可见:PlanSection 供 L 层把当前计划渲染进每轮消息尾部;
//   - 卡住提醒:Nudge 检测"有进行中任务却久未更新",在工具结果后附提醒。
//
// 计划按 (agent, session, 执行域) 隔离:子 agent 压执行域后与宿主分键,
// 互不覆盖。todo 只属于主循环(agent 与子 agent)——能结构化的任务用
// steps/引擎表达,不能预先结构化的任务流才需要外化计划。
//
// 存储由装配层构造并注入(New(kv, ttl)):消费方持有自己的后端,不读
// 任何全局单例——同进程多 agent 各持各的 todo 后端,互不覆盖。
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
)

type todoItem struct {
	Content string `json:"content"`
	// ActiveForm 是进行中的现在时表述("正在检索日志"),供通道进度展示。
	ActiveForm string `json:"active_form,omitempty"`
	Status     string `json:"status"` // pending | in_progress | completed
}

const (
	maxTodoItems      = 50
	maxTodoContentLen = 500
	nudgeAfterCalls   = 5 // 有进行中任务时,连续 N 次非 todo 调用触发提醒
	sep               = "\x1f"

	// 轮内状态袋(runctx.TurnState)的键前缀,拼上执行域键隔离:
	// written = 本轮写过计划;nagged = 本轮收口检查已催办过(每轮最多一次)。
	turnWritten = "todo.written" + sep
	turnNagged  = "todo.nagged" + sep
)

// todoState 是一个执行域的完整计划状态,整体作为一个 KV 值原子读改写:
// list 与 stale(nudge 卡住计数)同键,一次 Update 覆盖,多副本下不丢更新。
type todoState struct {
	List  []todoItem `json:"list"`
	Stale int        `json:"stale"` // 自上次计划更新以来的非 todo 工具调用数
}

// Todo 是 todo 计划外化的持有型对象:持有存储后端(store.KV)与保留时长,
// 由装配层用 New 注入。能力/提醒/计划渲染/清理都是它的方法,全部走
// t.kv,不读任何全局。ttl 为计划保留时长,0=不过期。
type Todo struct {
	kv  store.KV
	ttl time.Duration
}

// New 用注入的后端构造一个 todo 持有对象。
func New(kv store.KV, ttl time.Duration) *Todo {
	return &Todo{kv: kv, ttl: ttl}
}

// keyFor 用不可见分隔符拼接,agent 名/会话 ID/执行域含 "/" 也不碰撞。
func keyFor(agentName, sessionID, scope string) string {
	key := agentName + sep + sessionID
	if scope != "" {
		key += sep + scope
	}
	return key
}

// sessionKey 取 ctx 当前执行域的键。
func sessionKey(ctx context.Context) string {
	return keyFor(runctx.Agent(ctx), runctx.Session(ctx), runctx.Scope(ctx))
}

// loadState 读取一个执行域的计划状态,缺失/损坏返回空状态。
func (t *Todo) loadState(ctx context.Context, key string) todoState {
	b, ok, err := t.kv.Get(ctx, key)
	if err != nil || !ok {
		return todoState{}
	}
	var st todoState
	_ = json.Unmarshal(b, &st)
	return st
}

func encodeState(st todoState) []byte {
	b, _ := json.Marshal(st)
	return b
}

// firstInProgress 返回首个进行中任务的内容,无则空串。
func firstInProgress(list []todoItem) string {
	for _, t := range list {
		if t.Status == "in_progress" {
			return t.Content
		}
	}
	return ""
}

// validate 校验一次写入,返回给模型的纠正信息;通过返回空串。
func validate(items []todoItem) string {
	if len(items) > maxTodoItems {
		return fmt.Sprintf("写入被拒绝:任务数 %d 超过上限 %d。请合并同类项或分阶段规划。", len(items), maxTodoItems)
	}
	inProgress := 0
	seen := map[string]bool{}
	for i, t := range items {
		content := strings.TrimSpace(t.Content)
		if content == "" {
			return fmt.Sprintf("写入被拒绝:第 %d 项 content 为空。", i+1)
		}
		if len([]rune(content)) > maxTodoContentLen {
			return fmt.Sprintf("写入被拒绝:第 %d 项超过 %d 字符,任务描述应当简短。", i+1, maxTodoContentLen)
		}
		if seen[content] {
			return fmt.Sprintf("写入被拒绝:任务 %q 重复。", content)
		}
		seen[content] = true
		switch t.Status {
		case "pending", "completed":
		case "in_progress":
			inProgress++
		default:
			return fmt.Sprintf("写入被拒绝:第 %d 项 status 为 %q,只能是 pending|in_progress|completed。", i+1, t.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Sprintf("写入被拒绝:有 %d 项同时 in_progress。一次只做一件事:保持恰好一项进行中,其余 pending。", inProgress)
	}
	return ""
}

const todoWriteDesc = `写入/更新任务计划清单(整体替换)。使用规范:
- 何时用:任务需要 3 步以上、或用户给出多项要求时,开始动手前先列计划;简单问答不要用。
- 开始做某项前,先把它标为 in_progress(同时最多一项进行中,写入时强制校验)。
- 完成一项立刻标 completed,不要攒到最后一起标;做的过程中发现新任务,加入清单。
- 没有完成的事不许标 completed:测试失败、部分完成、被阻塞都保持 in_progress 并新增说明项。
- 一轮只调用一次;整体替换语义:每次提交完整清单。`

// Capabilities 返回 todo_write / todo_read 两个能力(闭包捕获 t.kv)。
func (t *Todo) Capabilities() []capability.Capability {
	writeMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "todo_write"},
		Description: todoWriteDesc,
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"todos": {
				Type: schema.Array, Required: true, Desc: "完整的任务清单",
				ElemInfo: &schema.ParameterInfo{
					Type: schema.Object,
					SubParams: map[string]*schema.ParameterInfo{
						"content":     {Type: schema.String, Desc: "任务内容(简短祈使句)", Required: true},
						"status":      {Type: schema.String, Desc: "状态", Enum: []string{"pending", "in_progress", "completed"}, Required: true},
						"active_form": {Type: schema.String, Desc: "进行中的现在时表述,如「正在检索日志」,用于进度展示"},
					},
				},
			},
		}),
	}
	write := capability.New(writeMeta, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Todos []todoItem `json:"todos"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("parse todos: %w", err)
		}
		if msg := validate(args.Todos); msg != "" {
			return msg, nil // 违规以结果回传纠正,循环不中断
		}
		key := sessionKey(ctx)
		if bag := runctx.TurnState(ctx); bag != nil {
			bag.Store(turnWritten+key, true) // 本轮动过计划(收口检查的触发前提)
		}
		if len(args.Todos) == 0 {
			if err := t.kv.Delete(ctx, key); err != nil {
				return "", err
			}
			return "计划已清空。", nil
		}
		// 整体替换:list 覆盖,stale 归零(更新计划即重置卡住计数)。
		err := t.kv.Update(ctx, key, func(_ []byte, _ bool) ([]byte, error) {
			return encodeState(todoState{List: args.Todos, Stale: 0}), nil
		}, t.ttl)
		if err != nil {
			return "", err
		}
		return render(args.Todos), nil
	})

	readMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "todo_read"},
		Description: "读取当前任务计划清单。",
		Params:      schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}
	read := capability.New(readMeta, func(ctx context.Context, _ string) (string, error) {
		list := t.loadState(ctx, sessionKey(ctx)).List
		if len(list) == 0 {
			return "计划为空。", nil
		}
		return render(list), nil
	})

	return []capability.Capability{write, read}
}

// PlanSection 渲染当前执行域的计划,供 L 层每轮注入消息尾部
// (loop.PromptLayers.Plan)。计划为空时返回空串(不注入)。
// 本轮尚未写过计划时(经 runctx.TurnState 判定),标注为"遗留计划"并
// 指示清理——否则旧问题的残留计划配上"全部完成前不要停"的祈使,等于
// 指示模型把旧账抄进新清单,pending 项跨问题无限累计。
func (t *Todo) PlanSection(ctx context.Context) string {
	key := sessionKey(ctx)
	list := t.loadState(ctx, key).List
	if len(list) == 0 {
		return ""
	}
	if bag := runctx.TurnState(ctx); bag != nil {
		if _, written := bag.Load(turnWritten + key); !written {
			return "# 遗留任务计划(来自之前的对话轮次,非本轮所列)\n" +
				"先判断与当前问题的关系:无关项用 todo_write 提交删除后的清单(全部无关就提交空 todos 清空);仍相关则继续推进并更新状态。\n" +
				render(list)
		}
	}
	return "# 当前任务计划(完成一项立刻用 todo_write 更新;全部完成前不要停)\n" + render(list)
}

// FinishCheck 是主循环的计划收口检查(装配层经 loop.CheckedFinish 注入):
// 模型即将以纯文本收尾时,若本轮写过计划且清单仍有非 completed 项,返回
// 纠正指令弹回一次(每轮最多一次,经轮内状态袋去重)——把"正文说完成了、
// 状态还是 pending"的漂移在轮内抹平,不靠模型自觉。本轮没动过计划
// (纯问答轮/残留计划未被认领)不催,残留由 PlanSection 的遗留标注处理。
func (t *Todo) FinishCheck(ctx context.Context) string {
	bag := runctx.TurnState(ctx)
	if bag == nil {
		return "" // 无轮语义(未经 agent 入口),不介入
	}
	key := sessionKey(ctx)
	if _, written := bag.Load(turnWritten + key); !written {
		return ""
	}
	if _, nagged := bag.Load(turnNagged + key); nagged {
		return ""
	}
	open := 0
	for _, item := range t.loadState(ctx, key).List {
		if item.Status != "completed" {
			open++
		}
	}
	if open == 0 {
		return ""
	}
	bag.Store(turnNagged+key, true)
	return fmt.Sprintf("[计划收口] 你即将结束本轮回答,但任务计划还有 %d 项未收口。"+
		"先用 todo_write 提交与实际一致的完整清单:已完成的标 completed;不再做或与本轮无关的直接删掉;"+
		"确实要后续轮次继续的保持原状,并在回答里说明进展到哪。然后再给出最终回答。", open)
}

// Nudge 给能力集套上计划卡住提醒(Ring 0):存在进行中任务时,连续
// nudgeAfterCalls 次非 todo 工具调用都没有更新计划,就在下一个工具结果后
// 附加提醒——纪律靠 harness 兜底,不靠模型自觉。
func (t *Todo) Nudge(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if isTodoTool(c.Meta().Ref) {
			out = append(out, c)
			continue
		}
		out = append(out, &nudged{inner: c, todo: t})
	}
	return out
}

func isTodoTool(ref capability.Ref) bool {
	return ref.Domain == "builtin" && strings.HasPrefix(ref.Name, "todo_")
}

type nudged struct {
	inner capability.Capability
	todo  *Todo
}

func (n *nudged) Meta() capability.Meta { return n.inner.Meta() }

func (n *nudged) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := n.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", n.inner.Meta().Ref)
	}
	return &nudgedTool{inner: inv, todo: n.todo}, nil
}

func (n *nudged) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		out, err := capability.Invoke(ctx, n.inner, argsJSON)
		if err != nil {
			return out, err
		}
		return n.todo.withNudge(ctx, out), nil
	}), nil
}

type nudgedTool struct {
	inner tool.InvokableTool
	todo  *Todo
}

func (t *nudgedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *nudgedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		return out, err
	}
	return t.todo.withNudge(ctx, out), nil
}

// withNudge 推进卡住计数,到阈值时在结果后附加提醒并重置。计数自增是
// 原子读改写(与 todo_write 竞争同键也不丢),阈值判定的输出经闭包捕获。
func (t *Todo) withNudge(ctx context.Context, result string) string {
	key := sessionKey(ctx)
	var current string
	fire := false
	err := t.kv.Update(ctx, key, func(old []byte, ok bool) ([]byte, error) {
		var st todoState
		if ok {
			_ = json.Unmarshal(old, &st)
		}
		current = firstInProgress(st.List)
		if current == "" {
			return old, nil // 没有进行中的任务,不催、原样写回
		}
		st.Stale++
		if st.Stale >= nudgeAfterCalls {
			st.Stale = 0
			fire = true
		}
		return encodeState(st), nil
	}, t.ttl)
	if err != nil || !fire {
		return result
	}
	return result + fmt.Sprintf(
		"\n\n[计划提醒] 任务「%s」已进行多步:若已完成,立刻用 todo_write 标记并推进下一项;若计划有变,更新清单。",
		current)
}

// Snapshot 返回某会话主执行域的计划渲染文本,供通道(飞书卡片等)展示进度。
func (t *Todo) Snapshot(agentName, sessionID string) string {
	list := t.loadState(context.Background(), keyFor(agentName, sessionID, "")).List
	if len(list) == 0 {
		return ""
	}
	return render(list)
}

// Clear 清空某会话主执行域的计划,供通道/运维主动终结。
func (t *Todo) Clear(agentName, sessionID string) {
	_ = t.kv.Delete(context.Background(), keyFor(agentName, sessionID, ""))
}

// ClearCurrent 清空 ctx 当前执行域的计划。组件级临时清单在调用结束时
// 用它即弃——草稿纸和窗口同生命周期,不留跨调用状态。
func (t *Todo) ClearCurrent(ctx context.Context) {
	_ = t.kv.Delete(ctx, sessionKey(ctx))
}

func render(list []todoItem) string {
	var sb strings.Builder
	for _, t := range list {
		switch t.Status {
		case "in_progress":
			label := t.Content
			if t.ActiveForm != "" {
				label = t.Content + "(" + t.ActiveForm + ")"
			}
			fmt.Fprintf(&sb, "◐ %s\n", label)
		case "completed":
			fmt.Fprintf(&sb, "☑ %s\n", t.Content)
		default:
			fmt.Fprintf(&sb, "☐ %s\n", t.Content)
		}
	}
	return sb.String()
}
