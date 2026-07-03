package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestFileStorePathCollision 验证 sanitize 改写过的会话 ID 带哈希后缀,
// "a/b" 与 "a_b" 不再串线;本来安全的 ID 保持原名(兼容旧文件)。
func TestFileStorePathCollision(t *testing.T) {
	st, err := NewFileStore(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Append(ctx, "a/b", schema.UserMessage("甲")); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ctx, "a_b", schema.UserMessage("乙")); err != nil {
		t.Fatal(err)
	}
	one, _ := st.Load(ctx, "a/b")
	two, _ := st.Load(ctx, "a_b")
	if len(one) != 1 || len(two) != 1 || one[0].Content != "甲" || two[0].Content != "乙" {
		t.Fatalf("sessions crossed: %v %v", one, two)
	}

	// 安全 ID:文件名保持原名(兼容既有会话文件)
	fs := st.(*fileStore)
	if got := fs.path("cli-session"); got != fs.dir+"/cli-session.jsonl" {
		t.Fatalf("safe id should keep plain filename, got %s", got)
	}
}

// TestInMemoryEviction 验证 inmemory 后端超限时淘汰最久未活跃会话。
func TestInMemoryEviction(t *testing.T) {
	st := NewInMemory(0).(*inMemory)
	ctx := context.Background()
	for i := 0; i <= maxInMemorySessions; i++ { // 超限 1 个
		if err := st.Append(ctx, fmt.Sprintf("s%d", i), schema.UserMessage("x")); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.sessions) != maxInMemorySessions {
		t.Fatalf("sessions = %d, want %d", len(st.sessions), maxInMemorySessions)
	}
	// 最早的 s0 被淘汰,最新的还在
	if msgs, _ := st.Load(ctx, "s0"); len(msgs) != 0 {
		t.Fatal("oldest session should be evicted")
	}
	if msgs, _ := st.Load(ctx, fmt.Sprintf("s%d", maxInMemorySessions)); len(msgs) != 1 {
		t.Fatal("latest session should survive")
	}
}
