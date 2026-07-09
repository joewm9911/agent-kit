package config

// TestLiveGoalCheck:目标达成核对(U4.1)的真机效果验证。
//
// 构造一个"易漏答"的多部分任务(三个独立子问题,各需一次工具调用),
// 挂三个返回已知事实的工具 + todo(GoalCheck 生效)。断言最终答案覆盖
// 三个事实(机器判 grounding),并统计是否发生了目标核对重生成。
//
// 从效果验证:GoalCheck 的价值 = 收口前对照原始目标逐条自查,把"只答了
// 一部分"的静默失败在轮内抹平。真模型驱动,SMOKE_LIVE + key 门控。
//
//	ZHIPU_API_KEY=$(security find-generic-password -a agent-kit -s zhipu-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveGoalCheck -v -count=1 -timeout 15m
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
	"github.com/joewm9911/agent-kit/runtime/loop"

	_ "github.com/joewm9911/agent-kit/impl/model/zhipu"
)

// countingModel 包一层统计 Generate 次数(目标核对重生成会多算一次)。
type countingModel struct {
	inner einomodel.ToolCallingChatModel
	calls *atomic.Int32
}

func (c *countingModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	c.calls.Add(1)
	return c.inner.Generate(ctx, msgs, opts...)
}
func (c *countingModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	c.calls.Add(1)
	return c.inner.Stream(ctx, msgs, opts...)
}
func (c *countingModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	inner, err := c.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &countingModel{inner: inner, calls: c.calls}, nil
}

func TestLiveGoalCheck(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启(真机测试需显式触发)")
	}
	key := os.Getenv("ZHIPU_API_KEY")
	if key == "" {
		t.Skip("无 ZHIPU_API_KEY")
	}
	ctx := context.Background()
	base, err := model.Build(ctx, "zhipu", map[string]any{"api_key": key})
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	retry := loop.RetryConfig{MaxAttempts: 4, BaseDelay: loop.Duration(3 * time.Second), MaxDelay: loop.Duration(30 * time.Second)}

	// 三个返回已知事实的工具(各对应一个子问题)。
	inv, _, _ := liveTool("get_inventory", "查询商品库存数量",
		strParam("product_id", "商品ID"), func(string) string { return "库存 42 件" })
	price, _, _ := liveTool("get_price", "查询商品售价",
		strParam("product_id", "商品ID"), func(string) string { return "售价 199 元" })
	refund, _, _ := liveTool("get_refunds", "查询近7天退款笔数",
		capability.NoParams, func(string) string { return "近7天退款 3 笔" })

	// 三部分任务,易漏答:模型常只答前一两部分。
	const task = "帮我一次性查清三件事:1) 商品 P100 的库存;2) 商品 P100 的售价;" +
		"3) 近7天的退款笔数。三个都要在最终回答里给出具体数字。"

	// 覆盖判据(机器判 grounding):三个事实的关键数字都得出现。
	covers := func(ans string) (int, []string) {
		want := map[string]string{"库存 42": "42", "售价 199": "199", "退款 3": "3 笔"}
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

	// 干净的 A/B:两臂都开 todo(隔离 GoalCheck 的边际效应,不与整套 todo
	// 机制混淆),仅切换 capabilities.goal_check。各跑 runs 轮,对比全覆盖率
	// 与模型调用数。这是"从效果验证"的核心:GoalCheck 到底是净正还是净负。
	const runs = 3
	run := func(goalCheck bool) (fullCover int, totalCalls int32) {
		gc := goalCheck
		for i := 0; i < runs; i++ {
			var calls atomic.Int32
			m := &countingModel{inner: loop.RetryModel(base, retry), calls: &calls}
			ac := &AgentConfig{Name: "gc"}
			ac.Capabilities.GoalCheck = &gc // todo 默认开;仅切 GoalCheck
			a, err := buildAgent(ctx, ac, Profile{}, []capability.Capability{inv, price, refund}, nil, m, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Run(ctx, "gc-live", task)
			if err != nil {
				t.Fatalf("run(goal_check=%v) %d: %v", goalCheck, i, err)
			}
			hit, miss := covers(out)
			if hit == 3 {
				fullCover++
			}
			totalCalls += calls.Load()
			t.Logf("[goal_check=%v run %d] coverage %d/3 miss=%v modelCalls=%d", goalCheck, i+1, hit, miss, calls.Load())
		}
		return fullCover, totalCalls
	}

	onCover, onCalls := run(true)
	offCover, offCalls := run(false)
	t.Logf("A/B(todo 均开,仅切 GoalCheck):开 全覆盖 %d/%d 共 %d 次调用;关 全覆盖 %d/%d 共 %d 次调用",
		onCover, runs, onCalls, offCover, runs, offCalls)

	// 记录性断言(观测优先,不硬挡):GoalCheck 开着时不该整批漏答;
	// 覆盖率净变化和调用成本记进日志,由人/eval 判是否值得默认开。
	if onCover == 0 {
		t.Fatalf("GoalCheck 开却整批 0 覆盖,疑似机制异常")
	}
}
