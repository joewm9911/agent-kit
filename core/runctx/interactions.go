package runctx

// interactions.go:本轮用户交互记录(ask_user 的问答)。它们是真实的
// 用户对话,不是内部机械——发生在子循环里也不该随子循环丢弃,否则
// 同会话下一轮同一 skill 会把同一个问题再问一遍(实测痛点)。记录挂
// TurnState,由 agent 收口时追加进会话历史,让下一轮的大脑看得见、
// 经参数把已知答案传给组件。

import (
	"context"
	"sync"
)

// Interaction 是一次用户问答。
type Interaction struct {
	Question string
	Answer   string
}

type interactionLog struct {
	mu    sync.Mutex
	items []Interaction
}

const keyInteractionLog = "interaction-log"

// RecordInteraction 记录一次用户问答(任意深度;TurnState 缺席时 no-op)。
func RecordInteraction(ctx context.Context, q, a string) {
	bag := TurnState(ctx)
	if bag == nil {
		return
	}
	v, _ := bag.LoadOrStore(keyInteractionLog, &interactionLog{})
	log := v.(*interactionLog)
	log.mu.Lock()
	log.items = append(log.items, Interaction{Question: q, Answer: a})
	log.mu.Unlock()
}

// Interactions 返回本轮已记录的用户问答(按发生序)。
func Interactions(ctx context.Context) []Interaction {
	bag := TurnState(ctx)
	if bag == nil {
		return nil
	}
	v, ok := bag.Load(keyInteractionLog)
	if !ok {
		return nil
	}
	log := v.(*interactionLog)
	log.mu.Lock()
	defer log.mu.Unlock()
	out := make([]Interaction, len(log.items))
	copy(out, log.items)
	return out
}
