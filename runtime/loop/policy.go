package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
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
type ApprovalState struct {
	Mode ApprovalMode

	rules      []compiledRule
	remember   bool
	mu         sync.Mutex
	remembered *lru[bool] // session|refKey → 放行与否(有界,最久未用淘汰)
}

// NewApprovalState 编译策略并构造运行态,规则非法时报错(fail fast)。
func NewApprovalState(mode ApprovalMode, policy ApprovalPolicy) (*ApprovalState, error) {
	st := &ApprovalState{Mode: mode, remember: policy.Remember, remembered: newLRU[bool](4096)}
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

// recall 查询会话级决策记忆。
func (st *ApprovalState) recall(session, refKey string) (allowed, ok bool) {
	if !st.remember {
		return false, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	allowed, ok = st.remembered.get(session + "|" + refKey)
	return allowed, ok
}

// memorize 记住一次"总是允许/拒绝"的决定(会话级)。
func (st *ApprovalState) memorize(session, refKey string, allowed bool) {
	if !st.remember {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.remembered.put(session+"|"+refKey, allowed)
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
