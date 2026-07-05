package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

func TestRollingSummaryPersistence(t *testing.T) {
	ctx := context.Background()
	m := testmodel.New() // 所有响应(回答与摘要)都是 "done"
	runner, err := engine.Build(ctx, "react", &engine.Assembly{Model: m})
	if err != nil {
		t.Fatal(err)
	}
	store := inmemSession(0)
	ag := New("a", "", runner, m, Options{
		Store: store, Window: 50,
		Compaction: loop.CompactionConfig{MaxMessages: 6, KeepRecent: 2},
	})

	// 5 轮对话 = 10 条消息,超过阈值 6,应触发滚动摘要
	for i := 0; i < 5; i++ {
		if _, err := ag.Run(ctx, "s1", "问题"); err != nil {
			t.Fatal(err)
		}
		ag.WaitCompactions() // 摘要已异步化,等在途任务落盘再断言
	}

	all, _ := store.(session.FullLoader).LoadAll(ctx, "s1")
	var markers int
	for _, msg := range all {
		if strings.HasPrefix(msg.Content, summaryTagPrefix) {
			markers++
		}
	}
	if markers == 0 {
		t.Fatal("expect rolling summary marker persisted")
	}
	// 原始消息不删除(审计保留):10 条原文 + 标记
	raw, covered := RawHistory(all)
	if len(raw) != 10 {
		t.Fatalf("raw history should be intact, got %d", len(raw))
	}
	if covered == 0 {
		t.Fatal("summary should cover some messages")
	}
	// 织入视图 = 摘要 + 未覆盖部分,应远小于全量
	view := sessionView(all)
	if len(view) >= len(raw) {
		t.Fatalf("view should be compacted: view=%d raw=%d", len(view), len(raw))
	}
	if !strings.Contains(view[0].Content, "done") { // 摘要文本来自假模型
		t.Fatalf("view head should be summary, got %q", view[0].Content)
	}
}

func TestSessionViewWithoutSummary(t *testing.T) {
	msgs := []*schema.Message{schema.UserMessage("a"), schema.AssistantMessage("b", nil)}
	if v := sessionView(msgs); len(v) != 2 {
		t.Fatalf("no summary → view = raw, got %d", len(v))
	}
}
