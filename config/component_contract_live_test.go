package config

// TestLiveComponentReturnContract:组件"返回值契约"的真机 A/B。
//
// 现象:component(react + todo)跑完后,最后一条消息常是"任务已完成"这类
// 状态陈述,而不是交付物本身。而组件的返回值就是这条消息(skill.go 里
// `return out.Content, nil`),它会被上游 graph 当数据插进下一步——一句状态
// 陈述直接污染数据流。
//
// 假设:L1 里 todo 那一整节("VERY frequently""mark completed IMMEDIATELY")
// 权重远高于 tail 里孤零零一句 "give the final answer synthesizing all tool
// results";且**没有任何一句**告诉子循环"你的最后一条消息是返回值"。
//
// 干净隔离:组件 prompt 刻意不含任何输出契约(不写"最后给出结论"),两臂都
// 开 todo、同一任务、同一工具面,**仅切换 L1 是否追加返回值契约段**,测它的
// 边际效应。判据 = 返回值里是否含三个具体事实(机器判 grounding)。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveComponentReturnContract -v -count=1 -timeout 20m
import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
)

// componentReturnContract 是待验证的 L1 追加段:仅子循环(component/skill)注入,
// 顶层 agent 不加——顶层的最后一条消息是"给人看的回答",组件的是"返回给调用
// 方的数据"。先在测试里验证边际效应,数据支持再决定是否接进框架。
const componentReturnContract = `

# Your final message is a return value
You are a component invoked by a caller, not a chat partner. Your final message is the
value returned to that caller and may be fed directly into downstream steps. It must BE
the deliverable itself — the complete result with its concrete findings. Never return a
status statement such as "done" or "the task is complete". Even when every planned task
is marked completed, the final message must still contain the full result.`

func TestLiveComponentReturnContract(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启(真机测试需显式触发)")
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

	// 三个返回已知事实的工具:交付物必须把这三个数字带出来。
	inv, _, _ := liveTool("get_inventory", "查询商品库存数量",
		strParam("product_id", "商品ID"), func(string) string { return "库存 42 件" })
	price, _, _ := liveTool("get_price", "查询商品售价",
		strParam("product_id", "商品ID"), func(string) string { return "售价 199 元" })
	refund, _, _ := liveTool("get_refunds", "查询近7天退款笔数",
		capability.NoParams, func(string) string { return "近7天退款 3 笔" })

	// 两种输入形态:
	//   plain —— 直白任务(对照)
	//   plan  —— 输入本身是一份"执行计划"文档(复现真实场景:上游 analysis_plan
	//            的产物当 input 传进执行组件)。怀疑正是它把模型推向"计划已完成"。
	const taskPlain = "查清三件事:1) 商品 P100 的库存;2) 商品 P100 的售价;3) 近7天的退款笔数。"
	const taskPlan = `用户问题: 帮我看看 P100 的经营情况

执行计划:
1. 调用 get_inventory 查询商品 P100 的库存
2. 调用 get_price 查询商品 P100 的售价
3. 调用 get_refunds 查询近7天的退款笔数

请按上述执行计划完成。`

	// 机器判 grounding:三个具体数字都得出现在返回值里。
	covers := func(ans string) (int, []string) {
		want := map[string]string{"库存42": "42", "售价199": "199", "退款3笔": "3"}
		hit := 0
		var miss []string
		for label, frag := range want {
			if strings.Contains(ans, frag) {
				hit++
			} else {
				miss = append(miss, label)
			}
		}
		return hit, miss
	}

	// 调用方的对话快照:模拟 graph 里上游 analysis_plan 已经产出计划的语境。
	// context: fork 会把它垫进组件上下文(ForkMessages)。
	callerConv := []*schema.Message{
		schema.UserMessage("帮我看看 P100 的经营情况"),
		schema.AssistantMessage("我已制定分析方案:1) 查库存 2) 查售价 3) 查近7天退款。下面交由执行组件按方案执行。", nil),
	}

	const runs = 3
	run := func(withContract bool, task string, fork bool) (fullCover, statusOnly int) {
		lp := "" // 空 = skill.go 取 DefaultLoopPrompt(Todo 开)
		if withContract {
			lp = loop.DefaultLoopPrompt + componentReturnContract
		}
		for i := 0; i < runs; i++ {
			// 组件 prompt 刻意不含输出契约,隔离 L1 的边际效应
			decl := &skill.Declaration{
				Kind:   "component",
				Name:   "t/exec",
				Engine: "react",
				Todo:   true,
				Prompt: promptVal("你是任务执行者。用给定工具完成用户的任务。"),
			}
			c, err := skill.Build(ctx, decl, skill.Deps{
				DefaultModel: loop.RetryModel(raw, retry),
				Capabilities: []capability.Capability{inv, price, refund},
				Todo:         componentTodo(),
				LoopPrompt:   lp,
			})
			if err != nil {
				t.Fatalf("build component: %v", err)
			}
			// P3:input → 用户消息(prompt 已是系统指令)
			ictx := runctx.WithInput(runctx.With(ctx, "t", "contract-live"), task)
			if fork { // 复现 step 的 context: fork —— 垫入调用方对话快照
				ictx = loop.WithConversationSnapshot(ictx, callerConv)
				ictx = runctx.WithForkContext(ictx)
			}
			out, err := capability.Invoke(ictx, c, `{}`)
			if err != nil {
				t.Fatalf("invoke(contract=%v) %d: %v", withContract, i, err)
			}
			hit, miss := covers(out)
			if hit == 3 {
				fullCover++
			}
			if hit == 0 {
				statusOnly++
			}
			t.Logf("[contract=%v run %d] coverage %d/3 miss=%v len=%d\n  out=%q",
				withContract, i+1, hit, miss, len([]rune(out)), clipStr(out, 200))
		}
		return fullCover, statusOnly
	}

	// 条件矩阵:输入形态 × context:fork × L1 返回值契约
	type cond struct {
		name     string
		task     string
		fork     bool
		contract bool
	}
	conds := []cond{
		{"plain", taskPlain, false, false},
		{"plan", taskPlan, false, false},
		{"plan+fork", taskPlan, true, false},         // ← 最接近真实场景
		{"plan+fork+contract", taskPlan, true, true}, // ← 契约能否救回
	}
	type cell struct{ cover, status int }
	res := map[string]cell{}
	for _, c := range conds {
		t.Logf("=== 条件=%s (fork=%v contract=%v) ===", c.name, c.fork, c.contract)
		cov, st := run(c.contract, c.task, c.fork)
		res[c.name] = cell{cov, st}
	}
	for _, c := range conds {
		t.Logf("结果 %-22s 全覆盖 %d/%d  空壳 %d", c.name, res[c.name].cover, runs, res[c.name].status)
	}

	// 观测优先、不硬挡:结论由数据说话。
	if res["plain"].cover == 0 {
		t.Fatalf("对照组零覆盖,疑似任务/工具面异常,实验不成立")
	}
}
