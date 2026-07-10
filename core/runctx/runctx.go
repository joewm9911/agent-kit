// Package runctx 定义贯穿一次运行的上下文信息:agent 名、会话 ID、
// 人机交互通道。所有运行时组件(todo、ask_user、审批、轨迹打点)从
// ctx 取这些信息,避免层层透传参数。
package runctx

import (
	"context"
	"sync"
)

type keyAgent struct{}
type keySession struct{}
type keyInteractor struct{}
type keyInput struct{}
type keyLoopInput struct{}
type keyVars struct{}
type keyPersona struct{}
type keyUser struct{}

// ApprovalRequest 描述一次待批准的改动性操作。
type ApprovalRequest struct {
	CapRef      string // 能力标识
	Description string // 能力描述
	Arguments   string // 即将执行的参数 JSON
}

// Interactor 是人机交互通道:CLI、飞书等各自实现。
// Ask 阻塞等待用户回答;Approve 阻塞等待批准结果。
type Interactor interface {
	Ask(ctx context.Context, question string) (string, error)
	Approve(ctx context.Context, req ApprovalRequest) (bool, error)
}

// With 注入一次运行的上下文信息。
func With(ctx context.Context, agentName, sessionID string) context.Context {
	ctx = context.WithValue(ctx, keyAgent{}, agentName)
	return context.WithValue(ctx, keySession{}, sessionID)
}

// WithInteractor 注入人机交互通道。
func WithInteractor(ctx context.Context, i Interactor) context.Context {
	return context.WithValue(ctx, keyInteractor{}, i)
}

// Agent 返回当前 agent 名,未注入时为空串。
func Agent(ctx context.Context) string {
	s, _ := ctx.Value(keyAgent{}).(string)
	return s
}

// Session 返回当前会话 ID,未注入时为空串。
func Session(ctx context.Context) string {
	s, _ := ctx.Value(keySession{}).(string)
	return s
}

// GetInteractor 返回人机交互通道,可能为 nil。
func GetInteractor(ctx context.Context) Interactor {
	i, _ := ctx.Value(keyInteractor{}).(Interactor)
	return i
}

type keyScope struct{}

// WithScopePush 在执行域栈上追加一段(子 agent 调用、组件调用等),
// 供按执行域隔离的运行时状态(todo 等)分键——子执行体不再与宿主
// 共享同一份状态。段间以不可见分隔符相连,天然防拼接碰撞。
func WithScopePush(ctx context.Context, seg string) context.Context {
	if cur := Scope(ctx); cur != "" {
		seg = cur + "\x1f" + seg
	}
	return context.WithValue(ctx, keyScope{}, seg)
}

// Scope 返回当前执行域,顶层为空串。
func Scope(ctx context.Context) string {
	s, _ := ctx.Value(keyScope{}).(string)
	return s
}

// WithInput 注入本轮用户输入,供记忆自动召回等运行时组件做检索。
func WithInput(ctx context.Context, input string) context.Context {
	return context.WithValue(ctx, keyInput{}, input)
}

// Input 返回本轮用户输入,未注入时为空串。
func Input(ctx context.Context) string {
	s, _ := ctx.Value(keyInput{}).(string)
	return s
}

// WithLoopInput 注入 loop 原始用户输入(顶层 agent.Run 设定一次,穿透所有
// 嵌套恒定不变,对应内置变量 {$user_input})。已设定则不覆盖——组件嵌套时
// 子组件的作用域输入(Input)可层层重设,而原始输入始终是最外层那句。
func WithLoopInput(ctx context.Context, input string) context.Context {
	if s, _ := ctx.Value(keyLoopInput{}).(string); s != "" {
		return ctx // set-once:不被嵌套覆盖
	}
	return context.WithValue(ctx, keyLoopInput{}, input)
}

// LoopInput 返回 loop 原始用户输入({$user_input});未注入时回落到 Input
// (顶层与作用域输入相等,向后兼容未走 WithLoopInput 的路径)。
func LoopInput(ctx context.Context) string {
	if s, _ := ctx.Value(keyLoopInput{}).(string); s != "" {
		return s
	}
	return Input(ctx)
}

// WithVars 注入模板变量袋(params + 内置 $input/$user_input/$user_id),供多
// 阶段引擎渲染其阶段提示词。每个组件调用边界按本次入参重设(与 Input 同
// 生命周期);嵌套时子组件覆盖为自己的一份。
func WithVars(ctx context.Context, vars map[string]string) context.Context {
	return context.WithValue(ctx, keyVars{}, vars)
}

// Vars 返回模板变量袋;未注入时为 nil(模板渲染对 nil 安全,占位符原样保留)。
func Vars(ctx context.Context) map[string]string {
	m, _ := ctx.Value(keyVars{}).(map[string]string)
	return m
}

// WithPersona 注入本次组件调用的身份指令(渲染后的组件 prompt)。组件的
// 每一次模型调用都应带上它:循环调用经 loop 的 PromptLayers 织入系统消息
// L2,多阶段引擎的阶段调用经 engine 的 stageSystem 前置——persona 是运行时
// 上下文,放 runctx 供两侧共读(engine 不 import loop 的分层约束所需)。
func WithPersona(ctx context.Context, persona string) context.Context {
	return context.WithValue(ctx, keyPersona{}, persona)
}

// Persona 返回本次组件调用的身份指令;未注入时为空串。
func Persona(ctx context.Context) string {
	s, _ := ctx.Value(keyPersona{}).(string)
	return s
}

// WithUser 注入终端用户身份(飞书 open_id、HTTP 请求的 user 字段等)。
// 长期记忆的用户级作用域据此隔离:未注入时"用户记忆"无处安放,
// 写入按配置 fail fast,而非静默落进共享池。
func WithUser(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, keyUser{}, userID)
}

// User 返回终端用户身份,未注入时为空串。
func User(ctx context.Context) string {
	s, _ := ctx.Value(keyUser{}).(string)
	return s
}

type keyTurnState struct{}

// WithTurnState 在一次用户轮的入口挂一个轮内共享的可变状态袋:同轮的
// 能力/守卫用它记"本轮已发生"类事实(todo 本轮已写入、收口已催办等),
// 轮结束随 ctx 即弃。子循环共享同一个袋,键应自带作用域(如拼上执行域)。
func WithTurnState(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyTurnState{}, &sync.Map{})
}

// TurnState 取当前轮的状态袋;不在轮内(未经 WithTurnState)返回 nil,
// 调用方应把 nil 当"无轮语义"降级处理,而非报错。
func TurnState(ctx context.Context) *sync.Map {
	m, _ := ctx.Value(keyTurnState{}).(*sync.Map)
	return m
}
