package inmemory

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestInMemoryEviction 验证 inmemory 后端超限时淘汰最久未活跃会话。
func TestInMemoryEviction(t *testing.T) {
	st := New(0).(*store)
	ctx := context.Background()
	for i := 0; i <= maxInMemorySessions; i++ { // 超限 1 个
		if err := st.Append(ctx, fmt.Sprintf("s%d", i), schema.UserMessage("x")); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.sessions) != maxInMemorySessions {
		t.Fatalf("sessions = %d, want %d", len(st.sessions), maxInMemorySessions)
	}
	if msgs, _ := st.Load(ctx, "s0"); len(msgs) != 0 {
		t.Fatal("oldest session should be evicted")
	}
	if msgs, _ := st.Load(ctx, fmt.Sprintf("s%d", maxInMemorySessions)); len(msgs) != 1 {
		t.Fatal("latest session should survive")
	}
}
