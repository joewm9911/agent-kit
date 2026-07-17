// examples/pipeline —— 无脑硬编排样例:把多个 agent 串成固定流程。
//
// 概念收敛后(docs/concept-convergence-plan.md),固定流程不在配置层
// 表达(steps 已移除),由宿主 Go 代码用 eino compose 编排:每个
// sub-agent 经 capability.AsLambda 变成图节点,顺序由边保证——
// **节点内有脑(agent 自主循环),节点间无脑(运行期没有大脑路由,
// 执行路径强保证)**。权限门禁前置、审计必落库这类硬确定性属于这里,
// 不属于提示词。
//
// 两个形态:
//  1. 顺序链:authz 门禁(纯代码,模型绕不过)→ 分析 agent → 审计戳;
//  2. 并行汇合:两个 agent 并发跑,纯代码合并(compose.Parallel)。
//
// 运行:
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	  go run ./examples/pipeline
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/cloudwego/eino/compose"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/skill"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
)

func main() {
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		log.Fatal("需要 MINIMAX_API_KEY(keychain: security find-generic-password -a agent-kit -s minimax-api-key -w)")
	}
	ctx := context.Background()
	m, err := model.Build(ctx, "minimax", map[string]any{
		"api_key": key, "base_url": "https://api.minimaxi.com/v1",
	})
	if err != nil {
		log.Fatal(err)
	}

	// 业务工具(本地 mock):agent 节点内部自主使用。
	getProduct := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "shop", Name: "get_product"},
		Description: "查询商品详情(价格与成本)",
		Params:      capability.SingleParam("sku", "商品 SKU"),
		Risk:        capability.RiskReadonly,
	}, func(context.Context, string) (string, error) {
		return `{"sku":"P200","price":199,"cost":120,"status":"在售"}`, nil
	})

	// sub-agent:同构隔离子循环(节点内有脑)。
	newAgent := func(name, persona string) capability.Capability {
		c, err := skill.BuildAgent(ctx, &skill.AgentDecl{
			Name:   "pipeline/" + name,
			Prompt: prompt.Value{Literal: persona},
			Params: map[string]capability.ParamDecl{"task": {Type: "string", Required: true}},
		}, skill.Deps{DefaultModel: m, Capabilities: []capability.Capability{getProduct}})
		if err != nil {
			log.Fatal(err)
		}
		return c
	}
	analyst := newAgent("analyst", "你是定价分析师。完成任务:{task}。用 get_product 查数据,给出两句话结论。")

	// ---- 形态一:顺序链——authz(纯代码门禁)→ agent → audit(纯代码落账)----
	authz := compose.InvokableLambda(func(_ context.Context, in string) (string, error) {
		// 真实场景:校验用户/参数,拒绝则返回错误,整条链停在这里——
		// 这是提示词给不了的强保证。
		fmt.Println("[authz] 权限校验通过")
		return fmt.Sprintf(`{"task":%q}`, in), nil // 组装 agent 入参
	})
	analystL, err := analyst.AsLambda(ctx)
	if err != nil {
		log.Fatal(err)
	}
	audit := compose.InvokableLambda(func(_ context.Context, in string) (string, error) {
		fmt.Println("[audit] 结论已落审计账")
		return in, nil
	})
	chain, err := compose.NewChain[string, string]().
		AppendLambda(authz).
		AppendLambda(analystL).
		AppendLambda(audit).
		Compile(ctx)
	if err != nil {
		log.Fatal(err)
	}
	out, err := chain.Invoke(ctx, "评估 P200 当前定价是否合理")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("== 顺序链结论 ==\n" + out)

	// ---- 形态二:并行汇合——两个 agent 并发,纯代码合并 ----
	pricing := newAgent("pricing", "你是定价分析师。任务:{task}。用 get_product 查数据,只回答定价角度的一句话结论。")
	stock := newAgent("stock", "你是库存分析师。任务:{task}。用 get_product 查状态,只回答上下架角度的一句话结论。")
	pricingL, _ := pricing.AsLambda(ctx)
	stockL, _ := stock.AsLambda(ctx)
	fanIn := compose.InvokableLambda(func(_ context.Context, in map[string]any) (string, error) {
		return fmt.Sprintf("定价视角:%v\n库存视角:%v", in["pricing"], in["stock"]), nil
	})
	toArgs := compose.InvokableLambda(func(_ context.Context, in string) (string, error) {
		return fmt.Sprintf(`{"task":%q}`, in), nil
	})
	par, err := compose.NewChain[string, string]().
		AppendLambda(toArgs).
		AppendParallel(compose.NewParallel().
			AddLambda("pricing", pricingL).
			AddLambda("stock", stockL)).
		AppendLambda(fanIn).
		Compile(ctx)
	if err != nil {
		log.Fatal(err)
	}
	out, err = par.Invoke(ctx, "看看 P200 的情况")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("== 并行汇合结论 ==\n" + out)
}
