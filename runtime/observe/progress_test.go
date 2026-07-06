package observe

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// TestProgressNestedSpan 验证内层模型调用(digest 摘要)经 observedGenerate
// 上报后,在 Progress 输出里渲染为独立的模型行——终结"外层大 span 盖住
// 内层调用"的黑箱。
func TestProgressNestedSpan(t *testing.T) {
	big := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "bigout"},
	}, func(context.Context, string) (string, error) {
		return strings.Repeat("库存流水;", 2000), nil // 超阈值触发消化
	})
	m := testmodel.New(schema.AssistantMessage("要点:库存充足", nil))
	caps := loop.DigestResults([]capability.Capability{big}, m, 3000)

	var sb strings.Builder
	ctx := callbacks.InitCallbacks(context.Background(),
		&callbacks.RunInfo{Name: "root"}, Progress(&sb))
	ctx = runctx.With(ctx, "a", "s")
	ctx = loop.WithResultStore(ctx, loop.NewResultStore(store.NewInMemory(), 0)) // 消化的前提:轮级结果暂存
	out, err := capability.Invoke(ctx, caps[0], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "要点:库存充足") {
		t.Fatalf("digest result: %.80s", out)
	}
	// 消化的内层模型调用必须以独立模型行可见
	if !strings.Contains(sb.String(), "模型") || !strings.Contains(sb.String(), "要点:库存充足") {
		t.Fatalf("nested digest span must render as a model line, got:\n%s", sb.String())
	}
}
