package skill

// delegate 治理边界(single-agent-mode-plan 批3)。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func delegateHost() []capability.Capability {
	search := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "search"}, Risk: capability.RiskReadonly,
	}, func(context.Context, string) (string, error) { return "found", nil })
	ask := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "builtin", Name: "ask_user"},
		Risk: capability.RiskReadonly, Tags: []string{capability.TagInteractive},
	}, func(context.Context, string) (string, error) { return "answer", nil })
	return []capability.Capability{search, ask}
}

func TestDelegateGovernance(t *testing.T) {
	m := testmodel.New(schema.AssistantMessage("子任务完成:P115 亏本在售", nil))
	d := NewDelegate(m, delegateHost(), DelegateConfig{MaxRounds: 3, MaxParallel: 2}, Deps{DefaultModel: m})
	ctx := context.Background()

	// task 必填
	out, err := capability.Invoke(ctx, d, `{}`)
	if err != nil || !strings.Contains(out, "task is required") {
		t.Fatalf("missing task must be reported, got %q err=%v", out, err)
	}
	// 未知工具名:点名 + 列可用(可用清单不含交互类 ask_user = 深度纪律)
	out, _ = capability.Invoke(ctx, d, `{"task":"扫描","tools":["nope"]}`)
	if !strings.Contains(out, "nope") || !strings.Contains(out, "search") {
		t.Fatalf("unknown tool must list available, got %q", out)
	}
	if strings.Contains(out, "ask_user") {
		t.Fatal("interactive tools must be excluded from the delegate pool")
	}
	// happy path:子循环终答原样返回
	out, err = capability.Invoke(ctx, d, `{"task":"扫描存储品类找亏本商品"}`)
	if err != nil || !strings.Contains(out, "P115") {
		t.Fatalf("delegate must return sub-loop final answer, got %q err=%v", out, err)
	}
}
