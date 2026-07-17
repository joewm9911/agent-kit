package config

// TestLiveNoReAskAcrossTurns:ask_user 问答落会话的真机验收。
// 轮1:补货 skill(子循环)不知道仓库 → ask_user → 用户答"仓A";
// 轮2:同会话再次补货 → 大脑应从 [用户交互记录] 拿到仓A 经参数传入,
// 子循环不再重问。尺子:轮2 的 Ask 调用数(n=3 会话,≥2 次为 0 通过)。
//
//	MINIMAX_API_KEY=... SMOKE_LIVE=1 go test ./config/ -run TestLiveNoReAsk -v -count=1 -timeout 15m
import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/askuser"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/impl/session/inmemory"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"
)

type countingInteractor struct{ asks *atomic.Int64 }

func (c countingInteractor) Ask(context.Context, string) (string, error) {
	c.asks.Add(1)
	return "仓A", nil
}
func (c countingInteractor) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	return true, nil
}

func TestLiveNoReAskAcrossTurns(t *testing.T) {
	m := liveModel(t)
	ctx := context.Background()

	const sessions = 3
	noReAsk := 0
	for i := 0; i < sessions; i++ {
		var asks atomic.Int64
		restock := capability.New(capability.Meta{
			Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: "do_restock"},
			Description: "执行补货",
			Params: mustParams(t, map[string]capability.ParamDecl{
				"sku": {Type: "string", Required: true}, "warehouse": {Type: "string", Required: true},
			}),
			Risk: capability.RiskReadonly,
		}, func(_ context.Context, args string) (string, error) {
			return "补货完成:" + args, nil
		})
		sk, err := skill.BuildAgent(ctx, &skill.AgentDecl{
			Name:        "live/restock",
			Description: "给指定商品补货;不知道目标仓库时先向用户确认",
			Params: map[string]capability.ParamDecl{
				"sku":       {Type: "string", Required: true},
				"warehouse": {Type: "string", Desc: "目标仓库,已知时传入即无需再问用户"},
			},
			Prompt: prompt.Value{Literal: "给 {sku} 补货。目标仓库:{warehouse}。若仓库为空或未知,先用 ask_user 向用户确认,再调用 do_restock 执行。"},
		}, skill.Deps{DefaultModel: m,
			Capabilities: []capability.Capability{restock, askuser.New()}})
		if err != nil {
			t.Fatal(err)
		}
		runner, err := engine.Build(ctx, "react", &engine.Assembly{
			Model: m, Capabilities: []capability.Capability{sk, askuser.New()}, MaxSteps: 6,
			Modifier: loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo}.Modifier(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ag := agent.New("reask", "", runner, m, agent.Options{
			Store: inmemory.New(0), Window: 20,
			Interactor: countingInteractor{asks: &asks},
		})
		sess := fmt.Sprintf("s-%d", i)
		answer1, err := ag.Run(ctx, sess, "给 P103 补 100 件")
		if err != nil {
			t.Logf("[sess %d turn1] err=%v", i+1, err)
			continue
		}
		t.Logf("[sess %d turn1] answer=%.120s", i+1, answer1)
		turn1Asks := asks.Load()
		answer2, err := ag.Run(ctx, sess, "再给 P103 补 50 件")
		if err != nil {
			t.Logf("[sess %d turn2] err=%v", i+1, err)
			continue
		}
		turn2Asks := asks.Load() - turn1Asks
		used := strings.Contains(answer2, "仓A")
		t.Logf("[sess %d] turn1 asks=%d turn2 asks=%d 沿用仓A=%v", i+1, turn1Asks, turn2Asks, used)
		if turn2Asks == 0 && turn1Asks >= 1 {
			noReAsk++
		}
	}
	t.Logf("复问验收(n=%d):轮2 零重问 %d/%d", sessions, noReAsk, sessions)
	if noReAsk < 2 {
		t.Fatalf("交互记录未阻止重问:%d/%d(记录未被大脑利用,检查 [用户交互记录] 注入)", noReAsk, sessions)
	}
}

func mustParams(t *testing.T, d map[string]capability.ParamDecl) *schema.ParamsOneOf {
	t.Helper()
	p, err := capability.ParamsSchema(d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
