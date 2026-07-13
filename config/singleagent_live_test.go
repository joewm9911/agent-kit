package config

// 单 agent 模式批3/批5 真机:动态委派行为观察 + 工具面阶梯。
//
//	MINIMAX_API_KEY=... SMOKE_LIVE=1 go test ./config/ -run 'TestLiveDelegate|TestLiveToolFaceLadder' -v -count=1 -timeout 30m

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"
)

func liveModel(t *testing.T) einomodel.ToolCallingChatModel {
	t.Helper()
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启")
	}
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		t.Skip("无 MINIMAX_API_KEY")
	}
	base := os.Getenv("SMOKE_MODEL_BASE")
	if base == "" {
		base = "https://api.minimaxi.com/v1"
	}
	raw, err := model.Build(context.Background(), "minimax", map[string]any{"api_key": key, "base_url": base})
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	retry := loop.RetryConfig{MaxAttempts: 4, BaseDelay: loop.Duration(3 * time.Second), MaxDelay: loop.Duration(30 * time.Second)}
	return loop.RetryModel(raw, retry)
}

// TestLiveDelegateParallelScan:三品类扫描任务 + delegate 可用,观察模型是否
// 自主委派并行;硬断言只压结果正确性(三个预埋异常都被找到),委派行为
// 记录为观察数据(n=3,行为不作硬门槛——工具描述引导,不强迫)。
func TestLiveDelegateParallelScan(t *testing.T) {
	m := liveModel(t)
	ctx := context.Background()

	mkScan := func(cat, anomaly string, calls *atomic.Int64) capability.Capability {
		return capability.New(capability.Meta{
			Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: "scan_" + cat},
			Description: fmt.Sprintf("扫描%s品类全部商品,返回定价与状态明细", cat),
			Params:      capability.NoParams,
			Risk:        capability.RiskReadonly,
		}, func(context.Context, string) (string, error) {
			calls.Add(1)
			return fmt.Sprintf(`{"category":%q,"items":[{"sku":"P1","ok":true},{"sku":%q,"issue":"亏本在售,成本高于售价"}]}`, cat, anomaly), nil
		})
	}

	const runs = 3
	delegated, solved := 0, 0
	for i := 0; i < runs; i++ {
		var a, b, c, dlgCalls atomic.Int64
		host := []capability.Capability{
			mkScan("audio", "PA9", &a), mkScan("storage", "PS9", &b), mkScan("network", "PN9", &c),
		}
		dlg := skill.NewDelegate(m, host, skill.DelegateConfig{MaxRounds: 4, MaxParallel: 3}, skill.Deps{DefaultModel: m})
		counted := capability.New(dlg.Meta(), func(ctx context.Context, args string) (string, error) {
			dlgCalls.Add(1)
			return capability.Invoke(ctx, dlg, args)
		})
		caps := append(append([]capability.Capability{}, host...), counted)
		runner, err := engine.Build(ctx, "react", &engine.Assembly{
			Model: m, Capabilities: caps, MaxSteps: 8,
			Modifier: loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo}.Modifier(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ag := agent.New("dlg-live", "", runner, m, agent.Options{})
		answer, err := ag.Run(ctx, fmt.Sprintf("dlg-%d", i), "同时扫描音频、存储、网络三个品类,找出所有亏本在售的商品,汇总 SKU 清单")
		if err != nil {
			t.Logf("[run %d] err=%v", i+1, err)
			continue
		}
		ok := strings.Contains(answer, "PA9") && strings.Contains(answer, "PS9") && strings.Contains(answer, "PN9")
		if ok {
			solved++
		}
		if dlgCalls.Load() > 0 {
			delegated++
		}
		t.Logf("[run %d] 三异常齐=%v delegate 调用=%d 直扫=%d/%d/%d", i+1, ok, dlgCalls.Load(), a.Load(), b.Load(), c.Load())
	}
	t.Logf("委派观察(n=%d):任务完成 %d/%d,使用 delegate %d/%d", runs, solved, runs, delegated, runs)
	if solved < runs {
		t.Fatalf("三异常必须全部找到(委派与否是策略,正确性是硬门槛):%d/%d", solved, runs)
	}
}

// TestLiveToolFaceLadder:工具面 8/16/32 阶梯下的选择准确率(批5 尺子)。
// 每档 n=6:一个目标工具埋在 N-1 个似是而非的干扰工具里,问一个只有
// 目标工具能答的问题,量"首个工具调用即命中目标"的比率。
func TestLiveToolFaceLadder(t *testing.T) {
	m := liveModel(t)
	ctx := context.Background()

	mkTool := func(i int, hit *atomic.Int64, target bool) capability.Capability {
		name := fmt.Sprintf("op_metric_%02d", i)
		desc := fmt.Sprintf("查询运营指标 %02d(库存周转/退货率等衍生指标)", i)
		if target {
			name = "refund_rate_query"
			desc = "查询指定品类近 30 天退货率"
		}
		return capability.New(capability.Meta{
			Ref: capability.Ref{Kind: "tool", Domain: "live", Name: name}, Description: desc,
			Params: capability.SingleParam("category", "品类"), Risk: capability.RiskReadonly,
		}, func(context.Context, string) (string, error) {
			if target {
				hit.Add(1)
				return `{"category":"音频","refund_rate_30d":"2.3%"}`, nil
			}
			return `{"metric":"n/a"}`, nil
		})
	}

	const runs = 6
	for _, n := range []int{8, 16, 32} {
		correct := 0
		for r := 0; r < runs; r++ {
			var hit atomic.Int64
			caps := []capability.Capability{mkTool(0, &hit, true)}
			for i := 1; i < n; i++ {
				caps = append(caps, mkTool(i, &hit, false))
			}
			runner, err := engine.Build(ctx, "react", &engine.Assembly{
				Model: m, Capabilities: caps, MaxSteps: 3,
				Modifier: loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo}.Modifier(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ag := agent.New(fmt.Sprintf("ladder-%d", n), "", runner, m, agent.Options{})
			answer, err := ag.Run(ctx, fmt.Sprintf("l%d-%d", n, r), "音频品类近 30 天退货率是多少?")
			if err != nil {
				t.Logf("[N=%d run %d] err=%v", n, r+1, err)
				continue
			}
			if hit.Load() >= 1 && strings.Contains(answer, "2.3") {
				correct++
			}
		}
		t.Logf("工具面阶梯 N=%d:命中 %d/%d", n, correct, runs)
	}
}
