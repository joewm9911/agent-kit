package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// stubRunner 以函数实现 engine.Runner。
type stubRunner struct {
	fn func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error)
}

func (s *stubRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	return s.fn(ctx, msgs)
}

func (s *stubRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := s.fn(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

var _ engine.Runner = (*stubRunner)(nil)

// TestTurnSerialization 验证同会话并发轮被串行化,历史不交错。
func TestTurnSerialization(t *testing.T) {
	var inFlight, peak int32
	runner := &stubRunner{fn: func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return schema.AssistantMessage("ok", nil), nil
	}}
	store := inmemSession(0)
	ag := New("a", "", runner, nil, Options{Store: store, Window: 50})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ag.Run(context.Background(), "same", "q"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&peak) != 1 {
		t.Fatalf("concurrent turns in same session = %d, want 1 (serialized)", peak)
	}
	// 历史完整成对:4 轮 = 8 条,无交错撕裂
	all, _ := store.(session.FullLoader).LoadAll(context.Background(), "same")
	if len(all) != 8 {
		t.Fatalf("history = %d messages, want 8", len(all))
	}
	for i, m := range all {
		want := schema.User
		if i%2 == 1 {
			want = schema.Assistant
		}
		if m.Role != want {
			t.Fatalf("interleaved history at %d: %s", i, m.Role)
		}
	}
}

// TestFailedTurnLeavesTrace 验证失败轮落痕:下一轮能看到上次的错误。
func TestFailedTurnLeavesTrace(t *testing.T) {
	boom := errors.New("upstream 500")
	runner := &stubRunner{fn: func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		return nil, boom
	}}
	store := inmemSession(0)
	ag := New("a", "", runner, nil, Options{Store: store, Window: 50})

	if _, err := ag.Run(context.Background(), "s1", "干活"); !errors.Is(err, boom) {
		t.Fatalf("expect original error, got %v", err)
	}
	all, _ := store.(session.FullLoader).LoadAll(context.Background(), "s1")
	if len(all) != 2 {
		t.Fatalf("failed turn should persist user+failure note, got %d", len(all))
	}
	if !strings.Contains(all[1].Content, "上一轮执行失败") || !strings.Contains(all[1].Content, "upstream 500") {
		t.Fatalf("failure note = %q", all[1].Content)
	}
}

// TestTurnHistorySharedViaCtx 验证一轮只读一次 store:runner 里能拿到
// ctx 共享的全量历史。
func TestTurnHistorySharedViaCtx(t *testing.T) {
	var seen int
	runner := &stubRunner{fn: func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		seen = len(loop.TurnHistory(ctx))
		return schema.AssistantMessage("ok", nil), nil
	}}
	store := inmemSession(0)
	ag := New("a", "", runner, nil, Options{Store: store, Window: 50})
	ctx := context.Background()
	if _, err := ag.Run(ctx, "s1", "第一轮"); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(ctx, "s1", "第二轮"); err != nil {
		t.Fatal(err)
	}
	if seen != 2 { // 第二轮开始时,store 里有第一轮的 user+assistant
		t.Fatalf("TurnHistory in ctx = %d messages, want 2", seen)
	}
}

// TestSummaryViewAnchorsFirstUserMessage 验证锚定保护:滚动摘要覆盖后,
// 会话首条用户消息原文仍常驻织入视图。
func TestSummaryViewAnchorsFirstUserMessage(t *testing.T) {
	all := []*schema.Message{
		schema.UserMessage("最初的任务:迁移订单库,注意 2020 年前的归档表不动"),
		schema.AssistantMessage("好的", nil),
		schema.UserMessage("继续"),
		schema.AssistantMessage("推进中", nil),
		makeSummaryMsg(4, "早期进展摘要"),
		schema.UserMessage("现在到哪了"),
	}
	_, view, synthetic := splitSummaryView(all)
	if synthetic != 2 {
		t.Fatalf("synthetic = %d, want 2 (summary+anchor)", synthetic)
	}
	if !strings.HasPrefix(view[0].Content, "[已有摘要]") {
		t.Fatalf("view[0] should be merge-labeled summary, got %q", view[0].Content)
	}
	if view[1].Role != schema.User || !strings.Contains(view[1].Content, "归档表不动") {
		t.Fatalf("view[1] should anchor the original task verbatim, got %+v", view[1])
	}
	if view[2].Content != "现在到哪了" {
		t.Fatalf("suffix wrong: %+v", view[2])
	}
}

// TestStreamAppendsAndCompacts 验证流式路径:聚合落盘 + 异步摘要,
// 且轮次锁在流耗尽后释放。
func TestStreamAppendsAndCompacts(t *testing.T) {
	runner := &stubRunner{fn: func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		return schema.AssistantMessage("流式回答", nil), nil
	}}
	store := inmemSession(0)
	ag := New("a", "", runner, nil, Options{
		Store: store, Window: 50,
		Compaction: loop.CompactionConfig{MaxMessages: 100},
	})
	sr, err := ag.Stream(context.Background(), "s1", "q")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, e := sr.Recv(); e != nil {
			break
		}
	}
	// 锁交接给聚合协程,下一轮 Run 会等它释放——能拿到锁即验证释放
	if _, err := ag.Run(context.Background(), "s1", "q2"); err != nil {
		t.Fatal(err)
	}
	ag.WaitCompactions()
	all, _ := store.(session.FullLoader).LoadAll(context.Background(), "s1")
	if len(all) != 4 {
		t.Fatalf("history = %d, want 4 (two turns)", len(all))
	}
}
