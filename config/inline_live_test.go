package config

// TestLiveInlineProcedureAB:单 agent 模式批2 的真机 A/B。
//
// 同一个四步定价审查流程(查详情→查销量→算毛利率→输出四数字结论),
// A 臂 subloop(现状:skill 子循环执行,终答经宿主转写),
// B 臂 inline(过程卡:宿主主循环照指引亲自执行)。
// 尺子(n 次采样):
//   - 完成率:终答覆盖四个锚点数字(价格 200 / 成本 120 / 销量 340 / 毛利率 40);
//   - 贯彻率:两个工具都真实调用(过程卡最大的风险是"知道了"一句带过);
//   - 模型调用数(成本对照,inline 应更少)。
// 通过标准(plan §7):inline 完成率 ≥ subloop 臂的 90%。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveInlineProcedureAB -v -count=1 -timeout 30m
import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"
)

func TestLiveInlineProcedureAB(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启")
	}
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		t.Skip("无 MINIMAX_API_KEY")
	}
	ctx := context.Background()
	base := os.Getenv("SMOKE_MODEL_BASE")
	if base == "" {
		base = "https://api.minimaxi.com/v1"
	}
	raw, err := model.Build(ctx, "minimax", map[string]any{"api_key": key, "base_url": base})
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	retry := loop.RetryConfig{MaxAttempts: 4, BaseDelay: loop.Duration(3 * time.Second), MaxDelay: loop.Duration(30 * time.Second)}

	const brief = `定价审查流程(严格按步骤):
1. 用 get_product 查商品 {sku} 的价格与成本;
2. 用 sales_summary 查该商品近 30 天销量;
3. 计算毛利率 =(价格-成本)/价格×100%;
4. 输出结论,必须包含四个数字:价格、成本、销量、毛利率(百分数)。`

	newTools := func(prodCalls, salesCalls *atomic.Int64) []capability.Capability {
		prod := capability.New(capability.Meta{
			Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: "get_product"},
			Description: "查询商品详情(价格与成本)",
			Params:      capability.SingleParam("sku", "商品 SKU"),
			Risk:        capability.RiskReadonly,
		}, func(context.Context, string) (string, error) {
			prodCalls.Add(1)
			return `{"sku":"P200","price":200,"cost":120,"status":"在售"}`, nil
		})
		sales := capability.New(capability.Meta{
			Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: "sales_summary"},
			Description: "查询商品近 30 天销量",
			Params:      capability.SingleParam("sku", "商品 SKU"),
			Risk:        capability.RiskReadonly,
		}, func(context.Context, string) (string, error) {
			salesCalls.Add(1)
			return `{"sku":"P200","units_30d":340}`, nil
		})
		return []capability.Capability{prod, sales}
	}

	anchors := []string{"200", "120", "340", "40"}
	type result struct {
		complete, followed int
		modelCalls         int64
	}

	runArm := func(name, mode string, runs int) result {
		var res result
		for i := 0; i < runs; i++ {
			var calls atomic.Int32
			var prodN, salesN atomic.Int64
			m := &countingModel{inner: loop.RetryModel(raw, retry), calls: &calls}
			tools := newTools(&prodN, &salesN)

			decl := &skill.Declaration{
				Name: "live/price_review", Mode: mode,
				Description: "按标准流程完成一次定价审查",
				Params:      map[string]capability.ParamDecl{"sku": {Type: "string", Required: true}},
				Prompt:      prompt.Value{Literal: brief},
			}
			// 有效形态:缺省已切 inline(纯 prompt+tools 推断为过程卡)
			effInline := mode == "inline" || mode == ""
			var deps skill.Deps
			deps.DefaultModel = m
			if !effInline {
				deps.Capabilities = tools // 子循环:工具在 skill 内部
			}
			card, err := skill.Build(ctx, decl, deps)
			if err != nil {
				t.Fatal(err)
			}
			hostCaps := []capability.Capability{card}
			if effInline {
				hostCaps = append(hostCaps, tools...) // 过程卡:工具直挂宿主
			}
			layers := loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo}
			runner, err := engine.Build(ctx, "react", &engine.Assembly{
				Model: m, Capabilities: hostCaps, MaxSteps: 8, Modifier: layers.Modifier(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ag := agent.New("ab-"+name, "", runner, m, agent.Options{})
			answer, err := ag.Run(ctx, fmt.Sprintf("%s-%d", name, i), "对 P200 做一次定价审查")
			if err != nil {
				t.Logf("[%s run %d] err=%v", name, i+1, err)
				continue
			}
			cov := 0
			for _, a := range anchors {
				if strings.Contains(answer, a) {
					cov++
				}
			}
			followed := prodN.Load() >= 1 && salesN.Load() >= 1
			if cov == len(anchors) {
				res.complete++
			}
			if followed {
				res.followed++
			}
			res.modelCalls += int64(calls.Load())
			t.Logf("[%s run %d] 锚点 %d/4 贯彻=%v 模型调用=%d answer_len=%d",
				name, i+1, cov, followed, calls.Load(), len([]rune(answer)))
		}
		return res
	}

	const runs = 6
	sub := runArm("subloop", "subloop", runs) // 缺省已切 inline,基线臂显式声明
	inl := runArm("inline", "", runs) // 裸声明:验证新缺省(纯 prompt+tools → inline)
	t.Logf("A/B(n=%d):subloop 完成 %d/%d 贯彻 %d/%d 模型调用均值 %.1f | inline 完成 %d/%d 贯彻 %d/%d 模型调用均值 %.1f",
		runs, sub.complete, runs, sub.followed, runs, float64(sub.modelCalls)/runs,
		inl.complete, runs, inl.followed, runs, float64(inl.modelCalls)/runs)
	// plan §7:inline 完成率 ≥ subloop 的 90%(n=6 粒度上取"不落后超过 1 次")
	if inl.complete < sub.complete-1 {
		t.Fatalf("inline 完成率显著落后基线:%d vs %d(inline 应收窄为微 skill 专用)", inl.complete, sub.complete)
	}
	if inl.followed < runs-1 {
		t.Fatalf("inline 贯彻率不足:%d/%d(过程卡被'知道了'带过,L1 纪律需要加强)", inl.followed, runs)
	}
}
