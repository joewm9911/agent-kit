package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
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

// TurnTerminal 标记轮次终止级错误(穿透工具错误兜底,见 engine)。
func (e *ErrBudgetExhausted) TurnTerminal() {}

func (e *ErrBudgetExhausted) Error() string {
	return "budget exhausted: " + e.Reason
}

// BudgetGate 是"配置 + 按会话累计"的预算门闸。它由 agent 在每次
// 运行时装入 ctx,BudgetModel 包装的模型从 ctx 读它扣费——因此
// skill/component 内部的模型调用也计入调用方 agent 的会话预算,
// 不再是治理盲区。
//
// 账目后端两种模式(装配层注入,消费方不感知):
//   - kv == nil:进程内有界 LRU(单副本默认,零依赖);
//   - kv != nil:落 store.KV(redis 等),同一会话打到不同副本共用一份
//     账目,预算是真正的分布式硬上限。键按 (agent, session) 隔离。
type BudgetGate struct {
	cfg      BudgetConfig
	mu       sync.Mutex
	sessions *lru[*spendSnap] // 进程内模式:有界,最久未活跃的会话账目被淘汰
	kv       store.KV
	ttl      time.Duration
}

// spendSnap 是会话账目快照(KV 模式整体 JSON 序列化,原子读改写)。
type spendSnap struct {
	Calls  int64 `json:"calls"`
	Tokens int64 `json:"tokens"`
}

// NewBudgetGate 创建预算门闸。零值配置 = 只统计不设限;kv 为 nil 用
// 进程内账目,非 nil 落外置后端(跨副本一致),ttl 为账目保留时长。
func NewBudgetGate(cfg BudgetConfig, kv store.KV, ttl time.Duration) *BudgetGate {
	return &BudgetGate{cfg: cfg, sessions: newLRU[*spendSnap](4096), kv: kv, ttl: ttl}
}

// bkey 是账目键:按 (agent, session) 隔离,多 agent 共享后端不碰撞。
func bkey(ctx context.Context) string {
	return "budget\x1f" + runctx.Agent(ctx) + "\x1f" + runctx.Session(ctx)
}

// Spend 返回某会话的累计花费(calls, tokens),供打点与计费。
func (g *BudgetGate) Spend(ctx context.Context) (int64, int64) {
	if g.kv != nil {
		raw, ok, err := g.kv.Get(ctx, bkey(ctx))
		if err != nil || !ok {
			return 0, 0
		}
		var s spendSnap
		_ = json.Unmarshal(raw, &s)
		return s.Calls, s.Tokens
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.sessions.get(bkey(ctx)); ok {
		return s.Calls, s.Tokens
	}
	return 0, 0
}

// exceeded 校验硬上限(纯函数,两种模式共用)。
func (g *BudgetGate) exceeded(s spendSnap) error {
	if g.cfg.MaxModelCalls > 0 && s.Calls >= int64(g.cfg.MaxModelCalls) {
		return &ErrBudgetExhausted{Reason: fmt.Sprintf("model calls reached %d", g.cfg.MaxModelCalls)}
	}
	if g.cfg.MaxTokens > 0 && s.Tokens >= int64(g.cfg.MaxTokens) {
		return &ErrBudgetExhausted{Reason: fmt.Sprintf("tokens reached %d", g.cfg.MaxTokens)}
	}
	return nil
}

// beginCall 原子记一次模型调用:先校验硬上限,再 calls+1,返回记账后
// 快照。KV 模式经后端原子读改写,多副本不丢账。
func (g *BudgetGate) beginCall(ctx context.Context) (spendSnap, error) {
	if g.kv != nil {
		var snap spendSnap
		err := g.kv.Update(ctx, bkey(ctx), func(old []byte, ok bool) ([]byte, error) {
			var s spendSnap
			if ok {
				_ = json.Unmarshal(old, &s)
			}
			if err := g.exceeded(s); err != nil {
				return nil, err
			}
			s.Calls++
			snap = s
			return json.Marshal(s)
		}, g.ttl)
		if err != nil && g.cfg.MaxModelCalls == 0 && g.cfg.MaxTokens == 0 {
			// 零限额 = 只统计不设限:后端故障时纯记账不该挡业务调用
			// (设了限额则维持 fail-closed——治理闸门宁停不放)。
			slog.Warn("budget: stats-only accounting failed, allowing call", "err", err)
			return snap, nil
		}
		return snap, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := bkey(ctx)
	s, ok := g.sessions.get(key)
	if !ok {
		s = &spendSnap{}
		g.sessions.put(key, s)
	}
	if err := g.exceeded(*s); err != nil {
		return spendSnap{}, err
	}
	s.Calls++
	return *s, nil
}

// addTokens 累计 token 用量(尽力而为:KV 故障不阻塞已产出的回答,
// 硬上限校验在下一次 beginCall 兜住)。
func (g *BudgetGate) addTokens(ctx context.Context, n int64) {
	if n <= 0 {
		return
	}
	if g.kv != nil {
		if err := g.kv.Update(ctx, bkey(ctx), func(old []byte, ok bool) ([]byte, error) {
			var s spendSnap
			if ok {
				_ = json.Unmarshal(old, &s)
			}
			s.Tokens += n
			return json.Marshal(s)
		}, g.ttl); err != nil {
			// 尽力而为是既定取舍(不阻塞已产出的回答),但丢账必须留痕:
			// 间歇故障下 MaxTokens 会持续少记、硬上限被静默放宽。
			slog.Warn("budget: token accounting lost", "tokens", n, "err", err)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.sessions.get(bkey(ctx)); ok {
		s.Tokens += n
	}
}

// nearLimit 报告是否已越过软阈值(任一维度 ≥80%)。
func (g *BudgetGate) nearLimit(s spendSnap) bool {
	if g.cfg.MaxModelCalls > 0 && s.Calls*10 >= int64(g.cfg.MaxModelCalls)*8 {
		return true
	}
	if g.cfg.MaxTokens > 0 && s.Tokens*10 >= int64(g.cfg.MaxTokens)*8 {
		return true
	}
	return false
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
	s, err := g.beginCall(ctx)
	if err != nil {
		return nil, err
	}
	if g.nearLimit(s) {
		msgs = append(msgs, schema.SystemMessage(
			"[预算提醒] 本次会话预算即将耗尽。请基于已获得的信息立即给出最终回答,不要再调用工具。"))
	}
	out, err := b.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	g.addTokens(ctx, countTokens(msgs, out))
	return out, nil
}

func (b *budgetModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	g := budgetFrom(ctx)
	if g == nil {
		return b.inner.Stream(ctx, msgs, opts...)
	}
	s, err := g.beginCall(ctx)
	if err != nil {
		return nil, err
	}
	g.addTokens(ctx, estimate(msgs)) // 流式:仅按输入估算,输出不阻塞统计
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
