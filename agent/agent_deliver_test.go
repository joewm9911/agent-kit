package agent

// direct 交付判定单测:独占轮次才替换,多 direct/非末次调用退化 attach。

import (
	"context"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

func TestDirectDeliverable(t *testing.T) {
	mk := func(items ...runctx.Deliverable) *runctx.DeliverableSink {
		_, s := runctx.WithDeliverableSink(context.Background())
		for range items {
			s.NextCallSeq()
		}
		for i, it := range items {
			it.Seq = i + 1
			s.Emit(it)
		}
		return s
	}

	// 唯一 direct 且是最后一次调用 → 触发
	s := mk(runctx.Deliverable{ID: "d1", Mode: capability.DeliverDirect, Content: "原文"})
	if d, ok := directDeliverable(s); !ok || d.Content != "原文" {
		t.Fatalf("sole trailing direct must fire, ok=%v", ok)
	}

	// direct 之后还有别的调用 → 不触发
	s = mk(
		runctx.Deliverable{ID: "d1", Mode: capability.DeliverDirect, Content: "原文"},
		runctx.Deliverable{ID: "d2", Mode: capability.DeliverAttach, Content: "证据"},
	)
	if _, ok := directDeliverable(s); ok {
		t.Fatal("direct followed by other calls must not fire")
	}

	// 多个 direct → 不触发
	s = mk(
		runctx.Deliverable{ID: "d1", Mode: capability.DeliverDirect, Content: "A"},
		runctx.Deliverable{ID: "d2", Mode: capability.DeliverDirect, Content: "B"},
	)
	if _, ok := directDeliverable(s); ok {
		t.Fatal("multiple directs must not fire")
	}

	// 无 direct → 不触发
	s = mk(runctx.Deliverable{ID: "d1", Mode: capability.DeliverAttach, Content: "A"})
	if _, ok := directDeliverable(s); ok {
		t.Fatal("attach must not fire direct")
	}
}
