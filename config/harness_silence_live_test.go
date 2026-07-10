package config

// TestLiveHarnessSilence:todo 段内嵌"返回值契约"的提示词真机 A/B。
//
// 复现:多步 + todo 的 component,MiniMax 约 1/3 概率把最终消息答成"所有任务已
// 完成"这类空壳、零实质内容。在 component 里这条空壳会成为返回值,污染下游数据流。
//
// 前两轮实验的教训(本轮修正):
//   ① 位置错——上次把静默规约放成离 todo 段很远的独立条目,无效;本轮把契约
//     焊进 todo 段内部、紧挨 "mark completed" 规则(空壳心智正是它训练出来的);
//   ② 尺子错——上次用正则词表判空壳,漏了"所有任务已完成"变体导致测量失真;
//     本轮尺子 = 覆盖率为 0(三个已查到的数字一个都没进最终消息),措辞无关;
//   ③ 守卫路径(CompletionNoticeGuard 正则拦截)已证是打地鼠,本轮两臂全关,
//     纯测提示词。
//
// A 臂 = 现状 L1;B 臂 = todo 段内嵌契约。同一 4 步 + todo 任务,n=8/臂。判据:
//   1. 空壳率是否下降;2. todo 机制不被压没(todoCountModel 数出参)。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveHarnessSilence -v -count=1 -timeout 30m
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
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
)

// todoCountModel 统计模型发起的 todo_write 调用(组件级 todo 调用结束即清空,
// 读 Snapshot 测不到,只能在模型出参处数)。
type todoCountModel struct {
	inner einomodel.ToolCallingChatModel
	todo  *atomic.Int32
}

func (c *todoCountModel) count(out *schema.Message) {
	if out == nil {
		return
	}
	for _, tc := range out.ToolCalls {
		if tc.Function.Name == "todo_write" {
			c.todo.Add(1)
		}
	}
}
func (c *todoCountModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	out, err := c.inner.Generate(ctx, msgs, opts...)
	c.count(out)
	return out, err
}
func (c *todoCountModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, err := c.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	cp := sr.Copy(2)
	sr1, sr2 := cp[0], cp[1]
	go func() {
		defer sr2.Close()
		var agg *schema.Message
		for {
			m, e := sr2.Recv()
			if e != nil {
				break
			}
			if agg == nil {
				agg = m
			} else if merged, me := schema.ConcatMessages([]*schema.Message{agg, m}); me == nil {
				agg = merged
			}
		}
		c.count(agg)
	}()
	return sr1, nil
}
func (c *todoCountModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	inner, err := c.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &todoCountModel{inner: inner, todo: c.todo}, nil
}


// harness 叙述词:出现即视为污染(中英各覆盖)。刻意避开会误伤正常业务表述的
// 泛词(如单独的"已完成"——"退款已完成"是合法答案)。
var narrationMarkers = []string{
	"已更新计划", "更新了计划", "更新计划", "计划已完成", "方案已完成", "任务已全部完成",
	"已保存到记忆", "已记录到记忆", "已存入记忆", "待办", "todo",
	"updated the plan", "saved to memory", "the plan is complete", "todo list",
}

func narrationHits(ans string) []string {
	low := strings.ToLower(ans)
	var hits []string
	for _, m := range narrationMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			hits = append(hits, m)
		}
	}
	return hits
}

func TestLiveHarnessSilence(t *testing.T) {
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

	inv, _, _ := liveTool("get_inventory", "查询商品库存数量",
		strParam("product_id", "商品ID"), func(string) string { return "库存 42 件" })
	price, _, _ := liveTool("get_price", "查询商品售价",
		strParam("product_id", "商品ID"), func(string) string { return "售价 199 元" })
	// 刻意返回超长文本(> DigestOver),触发结果消化 → 挂载并诱发 read_result。
	longRefund := strings.Repeat("退款流水记录若干,内容与结论无关。", 30) +
		"\n汇总:近7天退款 3 笔。\n" + strings.Repeat("其余流水略。", 30)
	refund, _, _ := liveTool("get_refunds", "查询近7天退款明细与汇总",
		capability.NoParams, func(string) string { return longRefund })

	// 四步、有依赖、多要求 —— 逼出 todo(否则测不到"叙述污染")。
	const task = "分步完成:1) 查商品 P100 的库存;2) 查商品 P100 的售价;3) 查近7天的退款明细;" +
		"4) 综合前三项数据,判断是否需要补货,给出结论和理由。每步做完再做下一步。"

	covers := func(ans string) (int, []string) {
		want := map[string]string{"库存42": "42", "售价199": "199", "退款3": "3"}
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

	// 契约行已按本实验数据落进 DefaultLoopPrompt(prompt.go loopPromptTodo)。
	// 保持可重跑:A 臂 = 剥掉契约行还原修复前的 L1;B 臂 = 现状(含契约)。
	// 契约行漂移即报错(逐字锚定,防止 L1 改了而实验静默失真)。
	const todoContractLine = "\n- Marking items complete is bookkeeping, not your answer. When the plan is done, your final message must still BE the result the user asked for — the data and conclusions themselves — never a status like \"all tasks completed\"."
	if !strings.Contains(loop.DefaultLoopPrompt, todoContractLine) {
		t.Fatalf("L1 已漂移:找不到 todo 返回值契约行,A/B 失去意义")
	}
	preFixL1 := strings.Replace(loop.DefaultLoopPrompt, todoContractLine, "", 1)

	// 本轮 A/B 测提示词:守卫两臂都关;空壳尺子 = 覆盖率为 0(hit==0),
	// 不用正则词表——前次实验正是词表漏了"所有任务已完成"变体导致测量失真。
	loop.CompletionNoticeGuard = false
	const runs = 8
	run := func(l1 string) (shell, todoUsed int) {
		for i := 0; i < runs; i++ {
			var todo atomic.Int32
			td := componentTodo()
			// 照 repo 里真实 todo 组件(deep_research)的写法:显式要求先列计划,
			// 保证 todo 机制被触发,才复现得了空壳收尾。
			decl := &skill.Declaration{
				Kind: "component", Name: "t/exec", Engine: "react", Todo: true,
				Prompt: promptVal("你是任务执行者。先用 todo_write 列出计划,逐项推进并更新状态,用给定工具完成用户的任务。"),
			}
			c, err := skill.Build(ctx, decl, skill.Deps{
				DefaultModel: loop.RetryModel(&todoCountModel{inner: raw, todo: &todo}, retry),
				Capabilities: []capability.Capability{inv, price, refund},
				Todo:         td,
				LoopPrompt:   l1, // A 臂空 = 现状 DefaultLoopPrompt;B 臂 = todo 段内嵌契约
				DigestOver:   200, // 触发消化 → 挂 read_result
			})
			if err != nil {
				t.Fatalf("build component: %v", err)
			}
			ictx := runctx.WithInput(runctx.With(ctx, "t", "silence-live"), task)
			out, err := capability.Invoke(ictx, c, `{}`)
			if err != nil {
				t.Fatalf("invoke %d: %v", i, err)
			}
			hit, _ := covers(out)
			// 空壳尺子 = 覆盖率为 0:三个已查到的数字一个都没进最终消息,
			// 无论措辞如何(所有任务已完成/分析结论已给出/…)都算空壳。
			isShell := hit == 0
			if isShell {
				shell++
			}
			if todo.Load() > 0 {
				todoUsed++
			}
			t.Logf("[run %d] coverage %d/3 空壳=%v todoCalls=%d len=%d\n  out=%q",
				i+1, hit, isShell, todo.Load(), len([]rune(out)), clipStr(out, 220))
		}
		return
	}

	defer func() { loop.CompletionNoticeGuard = true }() // 复位,别污染同包其他测试
	t.Logf("=== A 臂:修复前 L1(剥掉契约行)===")
	offShell, offTodo := run(preFixL1)
	t.Logf("=== B 臂:现状 L1(todo 段内嵌返回值契约)===")
	onShell, onTodo := run("") // 空 = DefaultLoopPrompt,已含契约
	t.Logf("A/B(守卫均关,仅切 L1 todo 段契约,同一 4 步+todo 任务,n=%d):", runs)
	t.Logf("  修复前 L1  空壳 %d/%d  todo 触发 %d/%d", offShell, runs, offTodo, runs)
	t.Logf("  现状 L1    空壳 %d/%d  todo 触发 %d/%d", onShell, runs, onTodo, runs)
	// 首轮实测(2026-07,MiniMax-M2.7,n=8/臂):修复前空壳 3/8,契约后 0/8,
	// todo 两臂均 8/8——契约行据此落入 DefaultLoopPrompt。

	// 实验有效性:todo 必须真触发,否则复现不了空壳,A/B 无意义。
	if offTodo == 0 && onTodo == 0 {
		t.Fatalf("两臂 todo 均未触发(%d/%d),任务没逼出计划机制,本次 A/B 无效", offTodo, runs)
	}
	// 契约不能把 todo 机制关掉(收敛写答案 ≠ 不用计划)。
	if onTodo == 0 && offTodo > 0 {
		t.Fatalf("契约把 todo 机制一起压没了(todo 从 %d/%d 掉到 0),这是退化", offTodo, runs)
	}
}
