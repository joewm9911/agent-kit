package loop

// read_result id 归一化 + miss 回报可用清单(排查"读不到结果"的两个真伤)。

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
)

func TestReadResultDirtyIDAndMissHint(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s1")
	rs := NewResultStore(store.NewInMemory(), 0)
	id := rs.Put(ctx, "big", strings.Repeat("原文", 5000))
	if id == "" {
		t.Fatal("put failed")
	}
	ctx = WithResultStore(ctx, rs)
	read := ReadResult()

	// 脏 id 形态必须都能取到
	for _, dirty := range []string{id, " " + id, id + " ", "结果" + id, strings.ToUpper(id)} {
		out, err := capability.Invoke(ctx, read, `{"id":"`+dirty+`"}`)
		if err != nil || !strings.Contains(out, "原文") {
			t.Fatalf("dirty id %q should resolve, got %q err=%v", dirty, out[:min(len(out), 80)], err)
		}
	}
	// miss 时回报可用清单
	out, _ := capability.Invoke(ctx, read, `{"id":"r99"}`)
	if !strings.Contains(out, id) {
		t.Fatalf("miss should list available ids, got %q", out)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
