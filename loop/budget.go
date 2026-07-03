package loop

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/runctx"
)

// BudgetConfig 是单个会话可花费的预算上限,零值字段不限制。
type BudgetConfig struct {
	MaxModelCalls int `yaml:"max_model_calls" json:"max_model_calls"`
	MaxTokens     int `yaml:"max_tokens" json:"max_tokens"`
}

// ErrBudgetExhausted 表示预算硬上限已到,循环被终止。
type ErrBudgetExhausted struct {
	Reason string
}

func (e *ErrBudgetExhausted) Error() string {
	return "budget exhausted: " + e.Reason
}

// BudgetGate 是"配置 + 按会话累计"的预算门闸。它由 agent 在每次
// 运行时装入 ctx,BudgetModel 包装的模型从 ctx 读它扣费——因此
// skill/component 内部的模型调用也计入调用方 agent 的会话预算,
// 不再是治理盲区。
type BudgetGate struct {
	cfg      BudgetConfig
	mu       sync.Mutex
	sessions map[string]*spend
}

type spend struct {
	calls  int64
	tokens int64
}

// NewBudgetGate 创建预算门闸。零值配置 = 只统计不设限。
func NewBudgetGate(cfg BudgetConfig) *BudgetGate {
	return &BudgetGate{cfg: cfg, sessions: map[string]*spend{}}
}

// Spend 返回某会话的累计花费(calls, tokens),供打点与计费。
func (g *BudgetGate) Spend(sessionID string) (int64, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.sessions[sessionID]; ok {
		return s.calls, s.tokens
	}
	return 0, 0
}

// check 校验硬上限并返回会话账目。
func (g *BudgetGate) check(session string) (*spend, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[session]
	if !ok {
		s = &spend{}
		g.sessions[session] = s
	}
	if g.cfg.MaxModelCalls > 0 && s.calls >= int64(g.cfg.MaxModelCalls) {
		return nil, &ErrBudgetExhausted{Reason: fmt.Sprintf("model calls reached %d", g.cfg.MaxModelCalls)}
	}
	if g.cfg.MaxTokens > 0 && s.tokens >= int64(g.cfg.MaxTokens) {
		return nil, &ErrBudgetExhausted{Reason: fmt.Sprintf("tokens reached %d", g.cfg.MaxTokens)}
	}
	return s, nil
}

// nearLimit 报告是否已越过软阈值(任一维度 ≥80%)。
func (g *BudgetGate) nearLimit(s *spend) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.MaxModelCalls > 0 && s.calls*10 >= int64(g.cfg.MaxModelCalls)*8 {
		return true
	}
	if g.cfg.MaxTokens > 0 && s.tokens*10 >= int64(g.cfg.MaxTokens)*8 {
		return true
	}
	return false
}

func (g *BudgetGate) add(s *spend, calls, tokens int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s.calls += calls
	s.tokens += tokens
}

type keyBudget struct{}

// WithBudget 把预算门闸装入 ctx,对下游所有 BudgetModel 包装的模型生效。
func WithBudget(ctx context.Context, g *BudgetGate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, keyBudget{}, g)
}

func budgetFrom(ctx context.Context) *BudgetGate {
	g, _ := ctx.Value(keyBudget{}).(*BudgetGate)
	return g
}

// BudgetModel 给模型套上预算控制(Ring 0):
//   - 硬上限:超出即返回 ErrBudgetExhausted,循环终止;
//   - 软阈值(80%):向输入追加收尾指令,让大脑尽快给出回答而非被硬断。
//
// 门闸从 ctx 读取(由 agent 每次运行装入),未装入时透传不设限。
// 预算按会话隔离,skill/component 内部调用同样计入。
func BudgetModel(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	return &budgetModel{inner: m}
}

type budgetModel struct {
	inner model.ToolCallingChatModel
}

func (b *budgetModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	g := budgetFrom(ctx)
	if g == nil {
		return b.inner.Generate(ctx, msgs, opts...)
	}
	s, err := g.check(runctx.Session(ctx))
	if err != nil {
		return nil, err
	}
	g.add(s, 1, 0)
	if g.nearLimit(s) {
		msgs = append(msgs, schema.SystemMessage(
			"[预算提醒] 本次会话预算即将耗尽。请基于已获得的信息立即给出最终回答,不要再调用工具。"))
	}
	out, err := b.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	g.add(s, 0, countTokens(msgs, out))
	return out, nil
}

func (b *budgetModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	g := budgetFrom(ctx)
	if g == nil {
		return b.inner.Stream(ctx, msgs, opts...)
	}
	s, err := g.check(runctx.Session(ctx))
	if err != nil {
		return nil, err
	}
	g.add(s, 1, estimate(msgs)) // 流式:仅按输入估算,输出不阻塞统计
	if g.nearLimit(s) {
		msgs = append(msgs, schema.SystemMessage(
			"[预算提醒] 本次会话预算即将耗尽。请基于已获得的信息立即给出最终回答,不要再调用工具。"))
	}
	return b.inner.Stream(ctx, msgs, opts...)
}

func (b *budgetModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := b.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &budgetModel{inner: inner}, nil
}

// countTokens 优先用平台回报的用量,缺失时按字符估算(约 3 字符/token)。
func countTokens(in []*schema.Message, out *schema.Message) int64 {
	if out.ResponseMeta != nil && out.ResponseMeta.Usage != nil {
		return int64(out.ResponseMeta.Usage.TotalTokens)
	}
	return estimate(in) + estimate([]*schema.Message{out})
}

// estimate 按字符类型分段估算 token:ASCII ≈ 4 字符/token,
// CJK 等宽字符 ≈ 0.75 token/字符。比笼统的字节除三更贴近主流
// tokenizer,预算与压缩阈值的触发时机随之校准。
func estimate(msgs []*schema.Message) int64 {
	var ascii, wide int
	for _, m := range msgs {
		for _, r := range m.Content {
			if r < 128 {
				ascii++
			} else {
				wide++
			}
		}
	}
	return int64(ascii/4) + int64(wide*3/4) + 1
}
