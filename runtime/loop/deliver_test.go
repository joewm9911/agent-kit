package loop

// 交付物捕获(Ring 0)单测:捕获/标记/降级/消化豁免/调用计数。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/store"
)

func deliverCap(name string, mode capability.DeliverMode, out string) capability.Capability {
	return capability.New(capability.Meta{
		Ref:     capability.Ref{Kind: "skill", Domain: "t", Name: name},
		Risk:    capability.RiskReadonly,
		Deliver: mode,
	}, func(context.Context, string) (string, error) { return out, nil })
}

func TestDeliverCaptureAndMarker(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	ctx, sink := runctx.WithDeliverableSink(ctx)
	rs := NewResultStore(store.NewInMemory(), 0)
	ctx = WithResultStore(ctx, rs)

	report := "# 月度报表\n|A|B|\n|1|2|"
	caps := DeliverResults([]capability.Capability{
		deliverCap("report", capability.DeliverAttach, report),
		deliverCap("qa", capability.DeliverNone, "证据结果"),
	})

	out, err := capability.Invoke(ctx, caps[0], "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[交付物#d1|report]") || !strings.Contains(out, report) {
		t.Fatalf("marked result must carry marker + full text, got %q", out[:80])
	}
	// 证据类:不捕获、无标记
	out2, _ := capability.Invoke(ctx, caps[1], "{}")
	if strings.Contains(out2, "交付物") {
		t.Fatalf("evidence must not be marked: %q", out2)
	}

	items := sink.Items()
	if len(items) != 1 || items[0].ID != "d1" || items[0].Content != report {
		t.Fatalf("sink items = %+v", items)
	}
	if items[0].Title != "月度报表" {
		t.Fatalf("title should come from first heading, got %q", items[0].Title)
	}
	if items[0].Seq != 1 || sink.LastCallSeq() != 2 {
		t.Fatalf("call seq bookkeeping wrong: seq=%d last=%d", items[0].Seq, sink.LastCallSeq())
	}
	// 原文可经 read_result 取回(d 系 id)
	if txt, ok, _ := rs.Get(ctx, "d1"); !ok || txt != report {
		t.Fatalf("d1 must be retrievable, ok=%v", ok)
	}
}

func TestDeliverDegradeWithoutStore(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	ctx, sink := runctx.WithDeliverableSink(ctx)
	// 不装 ResultStore:降级为轮内 id,本轮随行不受影响
	caps := DeliverResults([]capability.Capability{deliverCap("r", capability.DeliverAttach, "原文")})
	out, _ := capability.Invoke(ctx, caps[0], "{}")
	if !strings.Contains(out, "[交付物#d01|r]") {
		t.Fatalf("want turn-local id d01, got %q", out)
	}
	if items := sink.Items(); len(items) != 1 || items[0].ID != "d01" {
		t.Fatalf("sink = %+v", items)
	}
}

func TestDeliverNoSinkNoop(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	caps := DeliverResults([]capability.Capability{deliverCap("r", capability.DeliverAttach, "原文")})
	out, err := capability.Invoke(ctx, caps[0], "{}")
	if err != nil || strings.Contains(out, "交付物") {
		t.Fatalf("without sink capture must be a no-op, got %q err=%v", out, err)
	}
}

func TestDeliverSinkConcurrency(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	ctx, sink := runctx.WithDeliverableSink(ctx)
	caps := DeliverResults([]capability.Capability{deliverCap("r", capability.DeliverAttach, "x")})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = capability.Invoke(ctx, caps[0], "{}")
		}()
	}
	wg.Wait()
	if got := len(sink.Items()); got != 20 {
		t.Fatalf("want 20 captures, got %d", got)
	}
	if sink.LastCallSeq() != 20 {
		t.Fatalf("call seq = %d", sink.LastCallSeq())
	}
	ids := map[string]bool{}
	for _, it := range sink.Items() {
		if ids[it.ID] {
			t.Fatalf("duplicate id %s", it.ID)
		}
		ids[it.ID] = true
	}
}

// 消化不吞标记:超阈值的交付物,摘要头部保留 [交付物#dN] 行。
func TestDigestPreservesDeliverMarker(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	ctx, _ = runctx.WithDeliverableSink(ctx)
	ctx = WithResultStore(ctx, NewResultStore(store.NewInMemory(), 0))

	big := "# 大报表\n" + strings.Repeat("行数据|", 3000)
	inner := deliverCap("big", capability.DeliverAttach, big)
	caps := DeliverResults([]capability.Capability{inner})
	caps = DigestResults(caps, testmodel.New(schema.AssistantMessage("摘要要点", nil)), 1000, 0)

	out, err := capability.Invoke(ctx, caps[0], "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[交付物#d1|big]") {
		t.Fatalf("digest must preserve deliver marker at head, got %q", out[:min(len(out), 120)])
	}
	if !strings.Contains(out, "[结果已消化") && !strings.Contains(out, "[结果过长且消化失败") {
		t.Fatalf("oversized result should be digested, got %q", out[:min(len(out), 120)])
	}
}
