package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
)

// ApprovalRule 是一条参数级审批规则。静态 Risk 分级回答不了"同一个
// 工具因参数而异的危险性"(给自己人发消息 vs 对外发消息),规则把
// 放行策略下沉到 (能力, 参数) 粒度。
type ApprovalRule struct {
	// Ref 是 CapRef 模式(支持通配),空 = 任意能力。
	Ref string `yaml:"ref"`
	// Args 是参数名 → 值模式(精确、前缀 foo* 或 *),全部命中才算命中;
	// 参数缺失视为不命中。空 = 不看参数。
	Args map[string]string `yaml:"args"`
	// Action 是命中后的动作:allow(免批放行)| ask(照常审批)| deny(直接拒绝)。
	Action string `yaml:"action"`
}

// ApprovalPolicy 是 agent 的审批策略:规则自上而下首条命中生效,
// 无命中回落 ask。Remember 启用会话级决策记忆(用户选择"总是允许/
// 拒绝"后,同会话内同一能力不再重复询问)。
type ApprovalPolicy struct {
	Remember bool           `yaml:"remember"`
	Rules    []ApprovalRule `yaml:"rules"`
}

const (
	actionAllow = "allow"
	actionAsk   = "ask"
	actionDeny  = "deny"
)

type compiledRule struct {
	ref    *capability.Ref // nil = 任意
	args   map[string]string
	action string
}

// ApprovalState 是 agent 级的审批运行态:模式 + 编译后的策略 + 决策
// 记忆。由 agent 在每次运行装入 ctx,对主循环与 skill 内部统一生效。
//
// 决策记忆两种模式(装配层注入,消费方不感知):kv == nil 用进程内有界
// LRU(单副本默认);kv != nil 落 store.KV,"总是允许/拒绝"的决定跨副本
// 生效——同一会话打到另一副本不再重复询问。键按 (agent, session, 能力)
// 隔离。
type ApprovalState struct {
	Mode ApprovalMode

	rules      []compiledRule
	remember   bool
	mu         sync.Mutex
	remembered *lru[bool] // 进程内模式:session|refKey → 放行与否
	kv         store.KV
	ttl        time.Duration
	// promptMu 串行化交互式弹窗:并行工具调用会同时越过弹窗前的记忆
	// 检查排队弹窗,第一次"总是允许"救不了已排队的后续弹窗——获得此锁
	// 后必须锁内重查记忆(见 approval.go 的 gate)。
	promptMu sync.Mutex
}

// NewApprovalState 编译策略并构造运行态,规则非法时报错(fail fast)。
// kv 为 nil 时决策记忆留在进程内,非 nil 落外置后端(跨副本一致)。
func NewApprovalState(mode ApprovalMode, policy ApprovalPolicy, kv store.KV, ttl time.Duration) (*ApprovalState, error) {
	st := &ApprovalState{Mode: mode, remember: policy.Remember, remembered: newLRU[bool](4096), kv: kv, ttl: ttl}
	for i, r := range policy.Rules {
		cr := compiledRule{args: r.Args}
		switch r.Action {
		case actionAllow, actionAsk, actionDeny:
			cr.action = r.Action
		default:
			return nil, fmt.Errorf("approval_policy rule %d: bad action %q (want allow|ask|deny)", i, r.Action)
		}
		if r.Ref != "" && r.Ref != "*" {
			ref, err := capability.ParseRef(r.Ref)
			if err != nil {
				return nil, fmt.Errorf("approval_policy rule %d: %w", i, err)
			}
			cr.ref = &ref
		}
		st.rules = append(st.rules, cr)
	}
	return st, nil
}

// decide 按规则决定动作:首条命中生效,无命中回落 ask。
func (st *ApprovalState) decide(ref capability.Ref, argsJSON string) string {
	if len(st.rules) == 0 {
		return actionAsk
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)
	for _, r := range st.rules {
		if r.ref != nil && !ref.Match(*r.ref) {
			continue
		}
		if !matchArgs(r.args, args) {
			continue
		}
		return r.action
	}
	return actionAsk
}

func matchArgs(patterns map[string]string, args map[string]any) bool {
	for name, pat := range patterns {
		v, ok := args[name]
		if !ok {
			return false
		}
		if !matchValue(fmt.Sprint(v), pat) {
			return false
		}
	}
	return true
}

func matchValue(val, pat string) bool {
	if pat == "*" || pat == val {
		return true
	}
	if prefix, ok := strings.CutSuffix(pat, "*"); ok {
		return strings.HasPrefix(val, prefix)
	}
	return false
}

// akey 是决策记忆键:按 (agent, session, 能力) 隔离。
func akey(ctx context.Context, refKey string) string {
	return "approval\x1f" + runctx.Agent(ctx) + "\x1f" + runctx.Session(ctx) + "\x1f" + refKey
}

// recall 查询会话级决策记忆。
// promptSerialize/promptRelease 串行化交互式弹窗(见 gate 的锁内重查)。
func (st *ApprovalState) promptSerialize() { st.promptMu.Lock() }
func (st *ApprovalState) promptRelease()   { st.promptMu.Unlock() }

func (st *ApprovalState) recall(ctx context.Context, refKey string) (allowed, ok bool) {
	if !st.remember {
		return false, false
	}
	if st.kv != nil {
		raw, ok, err := st.kv.Get(ctx, akey(ctx, refKey))
		if err != nil || !ok || len(raw) == 0 {
			return false, false // 空值(外部误写)按未记忆:多问一次,不 panic
		}
		return raw[0] == '1', true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	allowed, ok = st.remembered.get(akey(ctx, refKey))
	return allowed, ok
}

// memorize 记住一次"总是允许/拒绝"的决定(会话级)。KV 故障时静默降级
// 为不记忆(失败模式安全:多问一次,不会放行未批准的操作)。
func (st *ApprovalState) memorize(ctx context.Context, refKey string, allowed bool) {
	if !st.remember {
		return
	}
	if st.kv != nil {
		v := []byte("0")
		if allowed {
			v = []byte("1")
		}
		_ = st.kv.Update(ctx, akey(ctx, refKey), func([]byte, bool) ([]byte, error) {
			return v, nil
		}, st.ttl)
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.remembered.put(akey(ctx, refKey), allowed)
}

type keyApprovalState struct{}

// WithApprovalState 把审批运行态装入 ctx。
func WithApprovalState(ctx context.Context, st *ApprovalState) context.Context {
	if st == nil {
		return ctx
	}
	return context.WithValue(ctx, keyApprovalState{}, st)
}

func approvalStateFrom(ctx context.Context) *ApprovalState {
	st, _ := ctx.Value(keyApprovalState{}).(*ApprovalState)
	return st
}

// Decision 是交互通道回传的审批决定。
type Decision int

const (
	DecisionDeny        Decision = iota // 本次拒绝
	DecisionAllow                       // 本次允许
	DecisionAlwaysAllow                 // 本会话总是允许该能力
	DecisionAlwaysDeny                  // 本会话总是拒绝该能力
)

// DecisionInteractor 是支持决策记忆的交互通道:比 Approve 的布尔多出
// "总是允许/拒绝"。实现该接口的通道自动获得记忆能力,未实现的回落
// Approve。
type DecisionInteractor interface {
	ApproveDecision(ctx context.Context, req runctx.ApprovalRequest) (Decision, error)
}
