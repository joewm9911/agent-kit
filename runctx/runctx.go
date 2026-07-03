// Package runctx 定义贯穿一次运行的上下文信息:agent 名、会话 ID、
// 人机交互通道。所有运行时组件(todo、ask_user、审批、轨迹打点)从
// ctx 取这些信息,避免层层透传参数。
package runctx

import "context"

type keyAgent struct{}
type keySession struct{}
type keyInteractor struct{}
type keyInput struct{}

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

// WithInput 注入本轮用户输入,供记忆自动召回等运行时组件做检索。
func WithInput(ctx context.Context, input string) context.Context {
	return context.WithValue(ctx, keyInput{}, input)
}

// Input 返回本轮用户输入,未注入时为空串。
func Input(ctx context.Context) string {
	s, _ := ctx.Value(keyInput{}).(string)
	return s
}
