package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/session"
)

// TestToolTrajectoryPersisted 验证工具轨迹随会话持久化:下一轮织入的
// 历史里能看到上一轮"做过什么、看到过什么"。
func TestToolTrajectoryPersisted(t *testing.T) {
	ctx := context.Background()
	weather := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "get_weather"},
	}, func(ctx context.Context, args string) (string, error) {
		return "北京 晴 25°C", nil
	})

	m := testmodel.New(
		testmodel.ToolCallMsg("get_weather", `{"city":"北京"}`),
		schema.AssistantMessage("今天北京晴,25 度。", nil),
	)
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        m,
		Capabilities: loop.RecordTools([]capability.Capability{weather}),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewInMemory(0)
	ag := New("a", "", runner, m, Options{
		Store: store, Window: 50, RecordTools: loop.RecordSummary,
	})

	if _, err := ag.Run(ctx, "s1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}

	all, _ := store.(session.FullLoader).LoadAll(ctx, "s1")
	if len(all) != 3 { // user + 执行记录 + assistant
		t.Fatalf("expect 3 messages, got %d", len(all))
	}
	tr := all[1]
	if !strings.Contains(tr.Content, "[执行记录]") ||
		!strings.Contains(tr.Content, "get_weather") ||
		!strings.Contains(tr.Content, "25°C") {
		t.Fatalf("trajectory message = %q", tr.Content)
	}

	// 下一轮织入的历史包含执行记录
	_, msgs, err := ag.loadTurn(ctx, "s1", "那上海呢")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "[执行记录]") {
			found = true
		}
	}
	if !found {
		t.Fatal("next turn history should include the trajectory")
	}
}

// TestRecordOffKeepsOldBehavior 验证 off 模式只存问答。
func TestRecordOffKeepsOldBehavior(t *testing.T) {
	ctx := context.Background()
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "echo"},
	}, func(ctx context.Context, args string) (string, error) { return "ok", nil })

	m := testmodel.New(
		testmodel.ToolCallMsg("echo", `{}`),
		schema.AssistantMessage("done", nil),
	)
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        m,
		Capabilities: loop.RecordTools([]capability.Capability{echo}),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewInMemory(0)
	ag := New("a", "", runner, m, Options{Store: store, Window: 50, RecordTools: loop.RecordOff})

	if _, err := ag.Run(ctx, "s1", "q"); err != nil {
		t.Fatal(err)
	}
	all, _ := store.(session.FullLoader).LoadAll(ctx, "s1")
	if len(all) != 2 {
		t.Fatalf("off mode should persist q&a only, got %d messages", len(all))
	}
}
