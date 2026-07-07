package config

// TestLiveEnginePrompts:D 类引擎内置提示词的真机格式遵循对照
// (prompt-inventory.md 打磨清单 P4)。此前这批提示词只有 testmodel
// 冒烟验证——testmodel 按脚本回放,验证不了"真模型是否遵循输出格式"。
//
// 覆盖 7 条内置提示词 × 在场的每个 provider:
//   - rewoo planner/solver:JSON 计划可解析、{eN} 引用被真实生成并替换
//     (机制硬断言:下游工具收到的参数 = 上游工具的产出);
//   - plan-execute planner/replanner/executor:两处 JSON 可解析、工具真实执行;
//   - reflection reviewer/executor:评审 JSON 可解析(引擎解析失败即报错);
//   - router route:选中且只选中语义匹配的目标,无 fallback 兜底。
//
// 分层断言纪律:引擎报错(=格式违约)与工具调用事实是硬断言;
// 最终答案的措辞(是否含具体数字)是软断言,失配只告警。
//
// 运行方式(key 只经环境变量;在场哪个 provider 跑哪个):
//
//	ZHIPU_API_KEY=$(security find-generic-password -a agent-kit -s zhipu-api-key -w) \
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveEnginePrompts -v -count=1 -timeout 30m

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"

	_ "github.com/joewm9911/agent-kit/impl/model/zhipu"
)

// engineLiveProviders 返回在场的真机 provider(key 环境变量决定)。
func engineLiveProviders(t *testing.T) map[string]func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
	t.Helper()
	retry := loop.RetryConfig{MaxAttempts: 4, BaseDelay: loop.Duration(3 * time.Second), MaxDelay: loop.Duration(30 * time.Second)}
	out := map[string]func(ctx context.Context) (einomodel.ToolCallingChatModel, error){}
	if key := os.Getenv("ZHIPU_API_KEY"); key != "" {
		out["zhipu/glm-5.2"] = func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
			m, err := model.Build(ctx, "zhipu", map[string]any{"api_key": key})
			if err != nil {
				return nil, err
			}
			return loop.RetryModel(m, retry), nil
		}
	}
	if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
		base := os.Getenv("SMOKE_MODEL_BASE")
		if base == "" {
			base = "https://api.minimaxi.com/v1"
		}
		name := os.Getenv("SMOKE_MINIMAX_MODEL") // 型号对照用;空 = 厂商默认 M2.7
		if name == "" {
			name = "MiniMax-M2.7"
		}
		out["minimax/"+name] = func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
			m, err := model.Build(ctx, "minimax", map[string]any{"api_key": key, "base_url": base, "model": name})
			if err != nil {
				return nil, err
			}
			return loop.RetryModel(m, retry), nil
		}
	}
	return out
}

// liveTool 构造带调用计数与末次参数记录的假工具(机制断言用)。
func liveTool(name, desc string, params *schema.ParamsOneOf, fn func(argsJSON string) string) (capability.Capability, *atomic.Int32, *atomic.Value) {
	var calls atomic.Int32
	var lastArgs atomic.Value
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: name},
		Description: desc,
		Params:      params,
	}
	c := capability.New(meta, func(_ context.Context, argsJSON string) (string, error) {
		calls.Add(1)
		lastArgs.Store(argsJSON)
		return fn(argsJSON), nil
	})
	return c, &calls, &lastArgs
}

func strParam(name, desc string) *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		name: {Type: schema.String, Desc: desc, Required: true},
	})
}

func TestLiveEnginePrompts(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启(真机测试需显式触发)")
	}
	providers := engineLiveProviders(t)
	if len(providers) == 0 {
		t.Skip("无可用 provider key(ZHIPU_API_KEY / MINIMAX_API_KEY)")
	}
	ctx := context.Background()

	for pname, build := range providers {
		t.Run(strings.ReplaceAll(pname, "/", "_"), func(t *testing.T) {
			m, err := build(ctx)
			if err != nil {
				t.Fatalf("build model: %v", err)
			}

			// —— rewoo:planner 的 JSON 计划 + {eN} 引用语法 + solver 汇总 ——
			t.Run("rewoo_planner_solver", func(t *testing.T) {
				top, topCalls, _ := liveTool("get_top_seller", "查询当前销量第一的商品,返回商品ID",
					capability.NoParams, func(string) string { return "销量第一的商品ID:P103" })
				inv, invCalls, invArgs := liveTool("get_inventory", "按商品ID查询库存数量",
					strParam("product_id", "商品ID,如 P103"), func(args string) string {
						if strings.Contains(args, "P103") {
							return "P103 当前库存 42 件"
						}
						return "(无此商品)"
					})
				r, err := engine.Build(ctx, "rewoo", &engine.Assembly{
					Model: m, Capabilities: []capability.Capability{top, inv},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := r.Generate(ctx, []*schema.Message{schema.UserMessage(
					"先查出销量第一的商品,再查它的库存,最后报告商品ID和库存数量。库存查询必须用第一步查到的商品ID。")})
				if err != nil {
					t.Fatalf("rewoo 引擎报错(planner 格式违约或引用非法): %v", err)
				}
				if topCalls.Load() == 0 || invCalls.Load() == 0 {
					t.Fatalf("计划未覆盖两个工具: top=%d inv=%d", topCalls.Load(), invCalls.Load())
				}
				// {eN} 替换的机制事实:库存工具收到的参数必须含上游产出的 P103
				if args, _ := invArgs.Load().(string); !strings.Contains(args, "P103") {
					t.Fatalf("{eN} 引用未生效,get_inventory 收到: %q", args)
				}
				if !strings.Contains(out.Content, "42") { // 软断言:solver 措辞
					t.Logf("[warn] solver 答案未含库存数 42: %q", out.Content)
				}
				t.Logf("[rewoo] %s", truncate(out.Content, 200))
			})

			// —— plan-execute:planner/replanner 两处 JSON + executor 真实调工具 ——
			t.Run("planexecute_planner_replanner_executor", func(t *testing.T) {
				weather, wCalls, _ := liveTool("get_weather", "按城市查询今天的天气",
					strParam("city", "城市名"), func(string) string { return "北京:晴,28°C,微风" })
				r, err := engine.Build(ctx, "plan-execute", &engine.Assembly{
					Model: m, Capabilities: []capability.Capability{weather},
					Config: map[string]any{"max_rounds": 2, "step_max_steps": 4},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := r.Generate(ctx, []*schema.Message{schema.UserMessage(
					"查询北京今天的天气,然后据此给出一句话穿衣建议。")})
				if err != nil {
					t.Fatalf("plan-execute 引擎报错(planner/replanner 格式违约): %v", err)
				}
				if wCalls.Load() == 0 {
					t.Fatal("executor 未真实调用天气工具")
				}
				if out.Content == "" {
					t.Fatal("最终回答为空")
				}
				if !strings.Contains(out.Content, "晴") && !strings.Contains(out.Content, "28") {
					t.Logf("[warn] 回答未含天气事实: %q", out.Content)
				}
				t.Logf("[plan-execute] %s", truncate(out.Content, 200))
			})

			// —— reflection:reviewer 每轮 JSON 判定(解析失败即引擎报错)——
			t.Run("reflection_reviewer_executor", func(t *testing.T) {
				facts, fCalls, _ := liveTool("get_product_facts", "查询产品事实(名称、发布年份)",
					capability.NoParams, func(string) string { return "产品名:极光耳机;发布年份:2026" })
				r, err := engine.Build(ctx, "reflection", &engine.Assembly{
					Model: m, Capabilities: []capability.Capability{facts},
					Config: map[string]any{"max_rounds": 2, "step_max_steps": 4},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := r.Generate(ctx, []*schema.Message{schema.UserMessage(
					"先查产品事实,再写一句产品宣传语,必须包含产品名和发布年份。")})
				if err != nil {
					t.Fatalf("reflection 引擎报错(reviewer 格式违约): %v", err)
				}
				if fCalls.Load() == 0 {
					t.Fatal("executor 未真实查询产品事实")
				}
				if !strings.Contains(out.Content, "2026") {
					t.Logf("[warn] 宣传语未含发布年份: %q", out.Content)
				}
				t.Logf("[reflection] %s", truncate(out.Content, 200))
			})

			// —— router:语义路由到唯一目标,无 fallback(选错/格式违约即报错)——
			t.Run("router_route", func(t *testing.T) {
				sales, sCalls, _ := liveTool("query_sales", "查询商品销量数据",
					strParam("query", "查询内容"), func(string) string { return "(销量数据)" })
				refunds, rCalls, _ := liveTool("query_refunds", "查询订单退款情况",
					strParam("query", "查询内容"), func(string) string { return "近7天退款 3 笔,金额 580 元" })
				r, err := engine.Build(ctx, "router", &engine.Assembly{
					Model: m, Capabilities: []capability.Capability{sales, refunds},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := r.Generate(ctx, []*schema.Message{schema.UserMessage("最近退款多不多?帮我看看退款情况。")})
				if err != nil {
					t.Fatalf("router 引擎报错(route 格式违约或目标名不存在): %v", err)
				}
				if rCalls.Load() != 1 || sCalls.Load() != 0 {
					t.Fatalf("路由目标错误: refunds=%d sales=%d", rCalls.Load(), sCalls.Load())
				}
				if !strings.Contains(out.Content, "退款") {
					t.Fatalf("路由结果未回流: %q", out.Content)
				}
				t.Logf("[router] %s", truncate(out.Content, 200))
			})
		})
	}
}
