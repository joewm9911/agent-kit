// Package builtin 提供框架内置能力:todo(计划外化)与 ask_user
// (人机交互)。
//
// todo 的纪律是 harness 强制的,不靠模型自觉,三道保证:
//   - 写入校验:状态枚举、最多一个 in_progress、内容规模,违规拒绝并纠正;
//   - 每轮可见:PlanSection 供 L 层把当前计划渲染进每轮消息尾部;
//   - 卡住提醒:NudgeTools 检测"有进行中任务却久未更新",在工具结果后附提醒。
//
// 计划按 (agent, session, 执行域) 隔离:子 agent 压执行域后与宿主分键,
// 互不覆盖。todo 只属于主循环(agent 与子 agent)——能结构化的任务用
// steps/引擎表达,不能预先结构化的任务流才需要外化计划。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
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
)

// todoStore 按 (agent, session, 执行域) 隔离的进程内计划存储。
type todoStore struct {
	mu    sync.Mutex
	lists map[string][]todoItem
	stale map[string]int // 自上次计划更新以来的非 todo 工具调用数(nudge 用)
}

var todos = &todoStore{lists: map[string][]todoItem{}, stale: map[string]int{}}

// sessionKey 用不可见分隔符拼接,agent 名/会话 ID/执行域含 "/" 也不碰撞。
func sessionKey(ctx context.Context) string {
	key := runctx.Agent(ctx) + sep + runctx.Session(ctx)
	if scope := runctx.Scope(ctx); scope != "" {
		key += sep + scope
	}
	return key
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

// TodoCapabilities 返回 todo_write / todo_read 两个能力。
func TodoCapabilities() []capability.Capability {
	writeMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "builtin", Name: "todo_write"},
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
		todos.mu.Lock()
		if len(todos.lists) > 4096 { // 粗粒度防泄漏
			todos.lists = map[string][]todoItem{}
			todos.stale = map[string]int{}
		}
		if len(args.Todos) == 0 {
			delete(todos.lists, key)
			delete(todos.stale, key)
			todos.mu.Unlock()
			return "计划已清空。", nil
		}
		todos.lists[key] = args.Todos
		todos.stale[key] = 0 // 更新计划即重置卡住计数
		todos.mu.Unlock()
		return render(args.Todos), nil
	})

	readMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "builtin", Name: "todo_read"},
		Description: "读取当前任务计划清单。",
		Params:      schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}
	read := capability.New(readMeta, func(ctx context.Context, _ string) (string, error) {
		todos.mu.Lock()
		list := todos.lists[sessionKey(ctx)]
		todos.mu.Unlock()
		if len(list) == 0 {
			return "计划为空。", nil
		}
		return render(list), nil
	})

	return []capability.Capability{write, read}
}

// PlanSection 渲染当前执行域的计划,供 L 层每轮注入消息尾部
// (loop.PromptLayers.Plan)。计划为空时返回空串(不注入)。
func PlanSection(ctx context.Context) string {
	todos.mu.Lock()
	list := todos.lists[sessionKey(ctx)]
	todos.mu.Unlock()
	if len(list) == 0 {
		return ""
	}
	return "# 当前任务计划(完成一项立刻用 todo_write 更新;全部完成前不要停)\n" + render(list)
}

// NudgeTools 给能力集套上计划卡住提醒(Ring 0):存在进行中任务时,
// 连续 nudgeAfterCalls 次非 todo 工具调用都没有更新计划,就在下一个
// 工具结果后附加提醒——纪律靠 harness 兜底,不靠模型自觉。
func NudgeTools(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if isTodoTool(c.Meta().Ref) {
			out = append(out, c)
			continue
		}
		out = append(out, &nudged{inner: c})
	}
	return out
}

func isTodoTool(ref capability.Ref) bool {
	return ref.Provider == "builtin" && strings.HasPrefix(ref.Name, "todo_")
}

type nudged struct {
	inner capability.Capability
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
	return &nudgedTool{inner: inv}, nil
}

func (n *nudged) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		out, err := capability.Invoke(ctx, n.inner, argsJSON)
		if err != nil {
			return out, err
		}
		return withNudge(ctx, out), nil
	}), nil
}

type nudgedTool struct {
	inner tool.InvokableTool
}

func (t *nudgedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *nudgedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		return out, err
	}
	return withNudge(ctx, out), nil
}

// withNudge 推进卡住计数,到阈值时在结果后附加提醒并重置。
func withNudge(ctx context.Context, result string) string {
	key := sessionKey(ctx)
	todos.mu.Lock()
	defer todos.mu.Unlock()
	list := todos.lists[key]
	current := ""
	for _, t := range list {
		if t.Status == "in_progress" {
			current = t.Content
			break
		}
	}
	if current == "" {
		return result // 没有进行中的任务,不催
	}
	todos.stale[key]++
	if todos.stale[key] < nudgeAfterCalls {
		return result
	}
	todos.stale[key] = 0
	return result + fmt.Sprintf(
		"\n\n[计划提醒] 任务「%s」已进行多步:若已完成,立刻用 todo_write 标记并推进下一项;若计划有变,更新清单。",
		current)
}

// Snapshot 返回某会话主执行域的计划渲染文本,供通道(飞书卡片等)展示进度。
func Snapshot(agentName, sessionID string) string {
	todos.mu.Lock()
	defer todos.mu.Unlock()
	list := todos.lists[agentName+sep+sessionID]
	if len(list) == 0 {
		return ""
	}
	return render(list)
}

// Clear 清空某会话主执行域的计划,供通道/运维主动终结。
func Clear(agentName, sessionID string) {
	key := agentName + sep + sessionID
	todos.mu.Lock()
	defer todos.mu.Unlock()
	delete(todos.lists, key)
	delete(todos.stale, key)
}

// ClearCurrent 清空 ctx 当前执行域的计划。组件级临时清单在调用结束时
// 用它即弃——草稿纸和窗口同生命周期,不留跨调用状态。
func ClearCurrent(ctx context.Context) {
	key := sessionKey(ctx)
	todos.mu.Lock()
	defer todos.mu.Unlock()
	delete(todos.lists, key)
	delete(todos.stale, key)
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
