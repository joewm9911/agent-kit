package config

// TestLiveDeliverReference:交付物直达通道的真机行为验证(批 4)。
//
// 机制侧(附件原文零损耗)是确定性代码,单测已锁;这里量的是**模型行为**:
// L1 引导句 + 结果标记之下,模型是否学会"引用 #dN 而非复述全文"。
// 判据(n 次采样):
//   - 引用率:终答含 #dN 引用(≥ 2/3 视为通过);
//   - 不复述:终答长度显著小于交付物原文(引用而非照抄);
//   - 机制保真:resolveDeliverables 取回的附件与原文逐字节一致(恒真,
//     顺带断言链路无损)。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveDeliverReference -v -count=1 -timeout 15m
import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/serving"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
)

func TestLiveDeliverReference(t *testing.T) {
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
	m := loop.RetryModel(raw, retry)

	// 交付物级能力:一份 30 行明细的销售报表(足够长,复述与引用可区分)。
	var rows strings.Builder
	rows.WriteString("# 键鼠外设 30 天销售报表\n\n| SKU | 商品 | 销量 | 销售额 | 趋势 | 毛利率 | 状态 |\n|---|---|---|---|---|---|---|\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&rows, "| P%03d | 商品%d | %d | ¥%d | +%d%% | %d%% | 在售 |\n", i, i, i*13, i*997, i%9, 20+i%30)
	}
	report := rows.String()

	reportCap := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "skill", Domain: "live", Name: "sales-report"},
		Description: "生成键鼠外设品类 30 天销售报表(完整明细)",
		Risk:        capability.RiskReadonly,
		Deliver:     capability.DeliverAttach,
	}, func(context.Context, string) (string, error) { return report, nil })

	caps := loop.DeliverResults([]capability.Capability{reportCap})
	layers := loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo}
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model: m, Capabilities: caps, MaxSteps: 4, Modifier: layers.Modifier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New("live-deliver", "", runner, m, agent.Options{})

	const runs = 6
	refHits, restates, verbatim := 0, 0, 0
	for i := 0; i < runs; i++ {
		turnCtx, sink := runctx.WithDeliverableSink(ctx)
		answer, err := ag.Run(turnCtx, fmt.Sprintf("deliver-%d", i), "给我出一份键鼠外设 30 天销售报表")
		if err != nil {
			t.Logf("[run %d] err=%v", i+1, err)
			continue
		}
		dels := serving.ResolveDeliverables(answer, sink)
		ref := strings.Contains(answer, "#d")
		restate := len([]rune(answer)) > len([]rune(report))*2/3 // 终答接近原文长度 = 照抄
		if ref {
			refHits++
		}
		if restate {
			restates++
		}
		if len(dels) == 1 && dels[0].Content == report {
			verbatim++
		}
		t.Logf("[run %d] 引用=%v 复述=%v 附件=%d answer_len=%d", i+1, ref, restate, len(dels), len([]rune(answer)))
	}
	t.Logf("A/B 结果:引用 %d/%d 复述 %d/%d 附件逐字节一致 %d/%d(原文 %d 字符)",
		refHits, runs, restates, runs, verbatim, runs, len([]rune(report)))
	if refHits*3 < runs*2 {
		t.Fatalf("引用率不足 2/3:%d/%d(L1 引导句未生效,需要调整措辞)", refHits, runs)
	}
	if verbatim != refHits {
		t.Fatalf("引用的轮次附件必须逐字节一致:verbatim=%d ref=%d", verbatim, refHits)
	}
}
