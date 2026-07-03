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

// BudgetTracker 按会话(runctx.Session)累计花费,可用于打点与计费。
type BudgetTracker struct {
	cfg      BudgetConfig
	mu       sync.Mutex
	sessions map[string]*spend
}

type spend struct {
	calls  int64
	tokens int64
}

func (t *BudgetTracker) get(ctx context.Context) *spend {
	key := runctx.Session(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[key]
	if !ok {
		s = &spend{}
		t.sessions[key] = s
	}
	return s
}

// Spend 返回某会话的累计花费(calls, tokens)。
func (t *BudgetTracker) Spend(sessionID string) (int64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		return s.calls, s.tokens
	}
	return 0, 0
}

// WrapModel 给模型套上预算控制:
//   - 硬上限:超出即返回 ErrBudgetExhausted,循环终止;
//   - 软阈值(80%):向输入追加收尾指令,让大脑基于已有结果尽快给出回答,
//     而不是被硬断在半途。
//
// 预算按会话隔离,同一 agent 服务多个会话互不影响。
func WrapModel(m model.ToolCallingChatModel, cfg BudgetConfig) (model.ToolCallingChatModel, *BudgetTracker) {
	t := &BudgetTracker{cfg: cfg, sessions: map[string]*spend{}}
	return &budgetModel{inner: m, t: t}, t
}

type budgetModel struct {
	inner model.ToolCallingChatModel
	t     *BudgetTracker
}

func (b *budgetModel) state(ctx context.Context) (*spend, error) {
	s := b.t.get(ctx)
	cfg := b.t.cfg
	b.t.mu.Lock()
	defer b.t.mu.Unlock()
	if cfg.MaxModelCalls > 0 && s.calls >= int64(cfg.MaxModelCalls) {
		return nil, &ErrBudgetExhausted{Reason: fmt.Sprintf("model calls reached %d", cfg.MaxModelCalls)}
	}
	if cfg.MaxTokens > 0 && s.tokens >= int64(cfg.MaxTokens) {
		return nil, &ErrBudgetExhausted{Reason: fmt.Sprintf("tokens reached %d", cfg.MaxTokens)}
	}
	return s, nil
}

// nearLimit 报告是否已越过软阈值(任一维度 ≥80%)。
func (b *budgetModel) nearLimit(s *spend) bool {
	cfg := b.t.cfg
	b.t.mu.Lock()
	defer b.t.mu.Unlock()
	if cfg.MaxModelCalls > 0 && s.calls*10 >= int64(cfg.MaxModelCalls)*8 {
		return true
	}
	if cfg.MaxTokens > 0 && s.tokens*10 >= int64(cfg.MaxTokens)*8 {
		return true
	}
	return false
}

func (b *budgetModel) prepare(s *spend, msgs []*schema.Message) []*schema.Message {
	if !b.nearLimit(s) {
		return msgs
	}
	return append(msgs, schema.SystemMessage(
		"[预算提醒] 本次会话预算即将耗尽。请基于已获得的信息立即给出最终回答,不要再调用工具。"))
}

func (b *budgetModel) add(s *spend, calls, tokens int64) {
	b.t.mu.Lock()
	defer b.t.mu.Unlock()
	s.calls += calls
	s.tokens += tokens
}

func (b *budgetModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	s, err := b.state(ctx)
	if err != nil {
		return nil, err
	}
	b.add(s, 1, 0)
	out, err := b.inner.Generate(ctx, b.prepare(s, msgs), opts...)
	if err != nil {
		return nil, err
	}
	b.add(s, 0, countTokens(msgs, out))
	return out, nil
}

func (b *budgetModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	s, err := b.state(ctx)
	if err != nil {
		return nil, err
	}
	b.add(s, 1, estimate(msgs)) // 流式:仅按输入估算,输出不阻塞统计
	return b.inner.Stream(ctx, b.prepare(s, msgs), opts...)
}

func (b *budgetModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := b.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &budgetModel{inner: inner, t: b.t}, nil // 共享同一 tracker
}

// countTokens 优先用平台回报的用量,缺失时按字符估算(约 3 字符/token)。
func countTokens(in []*schema.Message, out *schema.Message) int64 {
	if out.ResponseMeta != nil && out.ResponseMeta.Usage != nil {
		return int64(out.ResponseMeta.Usage.TotalTokens)
	}
	return estimate(in) + estimate([]*schema.Message{out})
}

func estimate(msgs []*schema.Message) int64 {
	var chars int
	for _, m := range msgs {
		chars += len(m.Content)
	}
	return int64(chars/3) + 1
}
