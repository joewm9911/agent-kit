// deliver.go:交付物收集器(交付物直达通道的 ctx 原语,设计见
// docs/deliverable-channel-plan.md)。Ring 0 的捕获中间件把标记能力的
// 原文 Emit 进来;出站层(serving/CLI/HTTP)按终答引用取走随行。
// 与 Interactor 同一注入模式:入口装 sink,不改 agent.Run 签名。
package runctx

import (
	"context"
	"fmt"
	"sync"

	"github.com/joewm9911/agent-kit/core/capability"
)

// Deliverable 是一份被捕获的交付物原文。
type Deliverable struct {
	ID      string // d<N>,轮内引用锚(经结果暂存后端时跨轮唯一)
	Title   string // 能力名(+ 内容首行标题启发)
	Source  string // 产出能力的完整 cap:// 引用
	Mode    capability.DeliverMode
	Content string
	Seq     int // 本轮全局工具调用序(direct 判定用:是否最后一次调用)
}

// DeliverableSink 是一轮运行的交付物收集器,并发安全(同轮并行工具)。
type DeliverableSink struct {
	mu      sync.Mutex
	items   []Deliverable
	callSeq int // 本轮工具调用总数(捕获中间件对每次调用递增)
	localID int // 后端不可用时的轮内降级 id 序
}

type keyDeliverableSink struct{}

// WithDeliverableSink 装入新收集器并返回它(出站入口调用:dispatcher /
// HTTP handler / CLI 宿主)。
func WithDeliverableSink(ctx context.Context) (context.Context, *DeliverableSink) {
	s := &DeliverableSink{}
	return context.WithValue(ctx, keyDeliverableSink{}, s), s
}

// EnsureDeliverableSink 复用已装入的收集器,缺席则装新的(agent.Run
// 调用:裸跑场景 direct 判定仍可用,出站方没装 sink 就没有随行呈现)。
func EnsureDeliverableSink(ctx context.Context) (context.Context, *DeliverableSink) {
	if s := DeliverableSinkFrom(ctx); s != nil {
		return ctx, s
	}
	return WithDeliverableSink(ctx)
}

// DeliverableSinkFrom 取出收集器,未装入返回 nil。
func DeliverableSinkFrom(ctx context.Context) *DeliverableSink {
	s, _ := ctx.Value(keyDeliverableSink{}).(*DeliverableSink)
	return s
}

// NextCallSeq 递增并返回本轮工具调用序号。nil 安全。
func (s *DeliverableSink) NextCallSeq() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callSeq++
	return s.callSeq
}

// LastCallSeq 返回当前已见的最大调用序号。nil 安全。
func (s *DeliverableSink) LastCallSeq() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callSeq
}

// Emit 收入一份交付物;ID 为空时分配轮内降级 id(后端不可用的路径,
// 本轮随行不受影响,只失去跨轮取回)。nil 安全(sink 缺席 = no-op)。
func (s *DeliverableSink) Emit(d Deliverable) Deliverable {
	if s == nil {
		return d
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == "" {
		s.localID++
		d.ID = fmt.Sprintf("d0%d", s.localID) // d0 前缀避开后端持久序的 dN
	}
	s.items = append(s.items, d)
	return d
}

// Items 返回已收集交付物的副本(按 Emit 序)。nil 安全。
func (s *DeliverableSink) Items() []Deliverable {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Deliverable(nil), s.items...)
}
