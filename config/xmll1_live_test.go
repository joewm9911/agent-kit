package config

// TestLiveXMLSectionsAB:L1 结构标签 A/B(参考 claude.ai 系统提示词的
// XML 语义分节,docs/context-architecture-plan 遗留项)。
//
// A 臂 = 现行 L1(Markdown `# 标题` 分节);
// B 臂 = 同一 L1 内容,每节转成 XML 语义信封(<tone_and_style>…
//        </completion_and_stopping>,顶层 <agent_behavior> 包裹)。
// 内容逐字节相同,只有结构不同——隔离"结构标签"这一个变量。
//
// 场景:interactive 电商工具面 + 多步销售诊断任务(需 sales_summary
// 全局扫描 → get_sales 下钻异常 SKU → 结构化结论)。
// 尺子(n 次采样):
//   - 完成率:终答含根因锚点(滞销 SKU 名 + "曝光");
//   - 贯彻率:诊断工具真实下钻到零销 SKU;
//   - 模型调用数(成本对照)。
// 判读:MiniMax 为主力,预期 XML 结构对非 Claude 模型无显著收益
//       (与注入对抗 A/B 同源假设);B 臂不劣于 A 即"可选、无害"。
//
// **负结果(2026-07-17,MiniMax-M2.7)**:两臂完全打平——完成 6/6=6/6、
// 下钻 6/6=6/6、调用均值 3.0=3.0,XML 结构零收益。这是"照搬 CC 提示词
// 形态对非 Claude 模型零收益"的第三次印证(前两次:注入对抗信封、
// L1 决策点提示)。结论:现行 Markdown 分节保持不变,不做 XML 化(纯
// 迁移成本无收益)。本测试与 xmlifyL1 保留为就绪工具:接入 Claude 系
// 模型时复跑,验证信封红利是否随受训模型成立(预期成立)。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveXMLSectionsAB -v -count=1 -timeout 30m
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

// xmlifyL1 把 Markdown `# 标题` 分节的 L1 转成 XML 语义分节。前言
// (首个 # 之前)进 <agent_behavior> 开头,每个 `# Section Name` 段
// 转为 <section_name>…</section_name>(空格转下划线、小写)。内容不改。
func xmlifyL1(md string) string {
	lines := strings.Split(md, "\n")
	var preamble []string
	type sec struct {
		name string
		body []string
	}
	var secs []sec
	cur := -1
	for _, ln := range lines {
		if strings.HasPrefix(ln, "# ") {
			title := strings.TrimPrefix(ln, "# ")
			tag := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(title), " ", "_"))
			secs = append(secs, sec{name: tag})
			cur = len(secs) - 1
			continue
		}
		if cur < 0 {
			preamble = append(preamble, ln)
		} else {
			secs[cur].body = append(secs[cur].body, ln)
		}
	}
	var sb strings.Builder
	sb.WriteString("<agent_behavior>\n")
	sb.WriteString(strings.TrimRight(strings.Join(preamble, "\n"), "\n"))
	for _, s := range secs {
		body := strings.Trim(strings.Join(s.body, "\n"), "\n")
		fmt.Fprintf(&sb, "\n\n<%s>\n%s\n</%s>", s.name, body, s.name)
	}
	sb.WriteString("\n</agent_behavior>")
	return sb.String()
}

func TestLiveXMLSectionsAB(t *testing.T) {
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

	// 自校验:转换后仍含所有分节标签,且前言保留。
	xmlL1 := xmlifyL1(loop.DefaultLoopPrompt)
	for _, want := range []string{"<agent_behavior>", "<tool_usage_policy>", "<completion_and_stopping>", "<task_management>", "continuous tool-use loop"} {
		if !strings.Contains(xmlL1, want) {
			t.Fatalf("xmlifyL1 缺 %q", want)
		}
	}

	newTools := func(drill *atomic.Int32) []capability.Capability {
		summary, _, _ := liveTool("sales_summary", "全店销售汇总:每个商品近30天总销量与销售额(units 为 0 = 滞销)",
			nil, func(string) string {
				return `[{"sku":"AUD-01","name":"降噪耳机","units":460,"revenue":59340},
{"sku":"AUD-02","name":"蓝牙音箱","units":0,"revenue":0},
{"sku":"AUD-03","name":"入耳式耳机","units":270,"revenue":21330}]`
			})
		series, _, _ := liveTool("get_sales", "查询单个商品近30天每日销量与渠道备注(诊断滞销/异常)",
			strParam("sku", "商品 SKU"), func(args string) string {
				if strings.Contains(args, "AUD-02") {
					drill.Add(1)
					return `{"sku":"AUD-02","daily_units_sum":0,"note":"7天前被移出首页曝光位,曝光量归零"}`
				}
				return `{"daily_units_sum":300,"note":"曝光正常"}`
			})
		return []capability.Capability{summary, series}
	}

	type result struct {
		complete, drilled int
		calls             int64
	}
	runArm := func(name, l1 string, runs int) result {
		var res result
		for i := 0; i < runs; i++ {
			var calls atomic.Int32
			var drill atomic.Int32
			m := &countingModel{inner: raw, calls: &calls}
			// todo 计划面在场(完整 L1),Modifier 挂计划注入。
			layers := loop.PromptLayers{Loop: l1}
			runner, err := engine.Build(ctx, "react", &engine.Assembly{
				Model: m, Capabilities: newTools(&drill), MaxSteps: 8,
				Modifier: layers.Modifier(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ag := agent.New("ab-"+name, "", runner, m, agent.Options{})
			answer, err := ag.Run(ctx, fmt.Sprintf("%s-%d", name, i),
				"分析音频品类近30天销售,找出滞销商品并诊断原因,给出结构化结论。")
			if err != nil {
				t.Logf("[%s run %d] err=%v", name, i+1, err)
				continue
			}
			d := drill.Load() >= 1
			c := strings.Contains(answer, "曝光") && (strings.Contains(answer, "蓝牙音箱") || strings.Contains(answer, "AUD-02"))
			if c {
				res.complete++
			}
			if d {
				res.drilled++
			}
			res.calls += int64(calls.Load())
			t.Logf("[%s run %d] 完成=%v 下钻=%v 模型调用=%d answer_len=%d",
				name, i+1, c, d, calls.Load(), len([]rune(answer)))
		}
		return res
	}

	const runs = 6
	a := runArm("markdown", loop.DefaultLoopPrompt, runs)
	b := runArm("xml", xmlL1, runs)
	t.Logf("结构标签 A/B(n=%d):Markdown 完成 %d/%d 下钻 %d/%d 调用均值 %.1f | XML 完成 %d/%d 下钻 %d/%d 调用均值 %.1f",
		runs, a.complete, runs, a.drilled, runs, float64(a.calls)/runs,
		b.complete, runs, b.drilled, runs, float64(b.calls)/runs)
	// 观察型:XML 不显著劣于 Markdown 即可(不落后超过 1 次)。
	if b.complete < a.complete-1 {
		t.Fatalf("XML 结构显著劣于 Markdown:完成 %d vs %d", b.complete, a.complete)
	}
}
