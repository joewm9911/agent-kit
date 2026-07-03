// Package testmodel 提供测试用的脚本化 ChatModel:按预设序列返回消息,
// 无需真实模型即可端到端验证循环、skill、workflow 等机制。
package testmodel

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Fake 按序返回 Responses;耗尽后返回固定收尾消息。
type Fake struct {
	mu        sync.Mutex
	Responses []*schema.Message
	Calls     int // 累计 Generate/Stream 次数
	i         int
}

// New 创建脚本化模型。
func New(responses ...*schema.Message) *Fake {
	return &Fake{Responses: responses}
}

// ToolCallMsg 构造一条发起工具调用的 assistant 消息。
func ToolCallMsg(toolName, argsJSON string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       "call-" + toolName,
		Type:     "function",
		Function: schema.FunctionCall{Name: toolName, Arguments: argsJSON},
	}})
}

func (f *Fake) next() *schema.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.i < len(f.Responses) {
		m := f.Responses[f.i]
		f.i++
		return m
	}
	return schema.AssistantMessage("done", nil)
}

// Generate 实现 model.BaseChatModel。
func (f *Fake) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return f.next(), nil
}

// Stream 实现 model.BaseChatModel。
func (f *Fake) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	out := f.next()
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// WithTools 实现 model.ToolCallingChatModel。
func (f *Fake) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}
