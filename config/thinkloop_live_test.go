package config

// TestLiveReadonlyFollowThroughAB:"建议形收尾"逃逸的度量尺 + L1 提示词
// 修法的负结果记录(2026-07-17)。
//
// 场景还原生产实测:报告类任务 + 多工具面,销售汇总里埋一个零销售
// SKU,诊断工具(get_sales)就在工具面上且能给出根因(移出首页)。
// 模型约有一半采样把"建议调用 get_sales 诊断"写进结果而不真调用。
//
// **负结果**:曾尝试 L1 加两句决策点提示(步间观察 + 只读下一步现在
// 就做),两轮 A/B(简单场景 5/6 vs 4/6;本场景 4/6 vs 3/6,合计 9/12
// vs 7/12)均无收益,按"修复必须 A/B 对照"家规回滚,不进 L1。
// 本测试保留为该失败模式的尺子:A 臂 = 现行 L1,B 臂 = 加两句的
// 变体(在测试内拼装);机制修复(守卫闸:终答推荐了工具面上未调用
// 的只读工具 → 弹回补跑)落地时,把 B 臂换成开守卫的形态复测。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveReadonlyFollowThroughAB -v -count=1 -timeout 30m
import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

func TestLiveReadonlyFollowThroughAB(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("set SMOKE_LIVE=1 and MINIMAX_API_KEY to run")
	}
	providers := engineLiveProviders(t)
	build, ok := providers["minimax/MiniMax-M2.7"]
	if !ok {
		t.Skip("无 MINIMAX_API_KEY")
	}
	ctx := context.Background()
	raw, err := build(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 工具面刻意加宽(生产形态:报告类长任务 + 多工具),让"建议形收尾"
	// 有发生条件;根因锚点只存在于诊断工具的返回里("移出首页")。
	newTools := func(diagnosed *atomic.Int32) []capability.Capability {
		summary, _, _ := liveTool("sales_summary", "全店销售汇总:每个商品近30天总销量与销售额",
			nil, func(string) string {
				return `[{"sku":"HZ-L1","name":"智能灯泡","units":0,"revenue":0},
{"sku":"HZ-S2","name":"智能插座","units":483,"revenue":33327},
{"sku":"HZ-T3","name":"温湿度传感器","units":720,"revenue":64080}]`
			})
		series, _, _ := liveTool("get_sales", "查询单个商品近30天的每日销量序列与渠道备注(诊断零销售/异常用)",
			strParam("sku", "商品 SKU"), func(args string) string {
				if strings.Contains(args, "HZ-L1") {
					diagnosed.Add(1)
					return `{"sku":"HZ-L1","daily_units_sum":0,"note":"该商品7天前被移出首页曝光位,此后曝光量降为0"}`
				}
				return `{"daily_units_sum":600,"note":"渠道曝光正常"}`
			})
		product, _, _ := liveTool("get_product", "按 SKU 查商品详情(售价与成本)",
			strParam("sku", "商品 SKU"), func(args string) string {
				switch {
				case strings.Contains(args, "HZ-L1"):
					return `{"sku":"HZ-L1","price":49,"cost":21}`
				case strings.Contains(args, "HZ-S2"):
					return `{"sku":"HZ-S2","price":69,"cost":34}`
				default:
					return `{"sku":"HZ-T3","price":89,"cost":47}`
				}
			})
		inventory, _, _ := liveTool("get_inventory", "查询商品库存明细",
			strParam("sku", "商品 SKU"), func(string) string {
				return `{"available":320,"reserved":12}`
			})
		return []capability.Capability{summary, series, product, inventory}
	}

	// A 臂:现行 L1;B 臂:加两句决策点提示的变体(负结果存证,见文件头)。
	const extraLines = `
- A tool result is information to weigh, not just progress: before the next call, check whether it changes your plan, and investigate anomalies it surfaces (a zero, an outlier, a contradiction) with your read-only tools instead of carrying them silently into the answer.
- If a recommendation's immediate next step is a read-only query your own tools can answer (a diagnosis, a verification, a lookup), run it now and fold the result into the answer. Leave a step as a suggestion only when it is a mutating action awaiting the user's decision, or needs information you cannot obtain.`
	oldL1 := loop.DefaultLoopPromptNoTodo
	newL1 := loop.DefaultLoopPromptNoTodo + extraLines

	type result struct{ diagnosed, anchored int; calls int64 }
	runArm := func(name, l1 string, runs int) result {
		var res result
		for i := 0; i < runs; i++ {
			var calls atomic.Int32
			var diag atomic.Int32
			m := &countingModel{inner: raw, calls: &calls}
			runner, err := engine.Build(ctx, "react", &engine.Assembly{
				Model: m, Capabilities: newTools(&diag), MaxSteps: 8,
				Modifier: loop.PromptLayers{Loop: l1}.Modifier(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ag := agent.New("ab-"+name, "", runner, m, agent.Options{})
			answer, err := ag.Run(ctx, fmt.Sprintf("%s-%d", name, i),
				"给我一份智能家居品类近30天的销售分析报告:每个商品的销量、销售额、毛利率,以及按优先级排列的提升毛利率建议。")
			if err != nil {
				t.Logf("[%s run %d] err=%v", name, i+1, err)
				continue
			}
			d := diag.Load() >= 1
			a := strings.Contains(answer, "首页") || strings.Contains(answer, "移出")
			if d {
				res.diagnosed++
			}
			if a {
				res.anchored++
			}
			res.calls += int64(calls.Load())
			t.Logf("[%s run %d] 诊断=%v 根因入答=%v 模型调用=%d answer_len=%d",
				name, i+1, d, a, calls.Load(), len([]rune(answer)))
		}
		return res
	}

	const runs = 6
	a := runArm("current", oldL1, runs)
	b := runArm("lines", newL1, runs)
	t.Logf("A/B(n=%d):现行L1 诊断 %d/%d 根因 %d/%d 调用均值 %.1f | 加两句 诊断 %d/%d 根因 %d/%d 调用均值 %.1f",
		runs, a.diagnosed, runs, a.anchored, runs, float64(a.calls)/runs,
		b.diagnosed, runs, b.anchored, runs, float64(b.calls)/runs)
	// 观察型度量尺:不设硬门槛(已知两臂都在 50-70% 区间,提示词修法
	// 无收益);守卫闸落地后把 B 臂换成开守卫形态,再上硬门槛。
}
