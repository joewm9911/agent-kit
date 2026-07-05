package file

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFileStoreRoundtripAndWindow(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, txt := range []string{"a", "b", "c"} {
		if err := s.Append(ctx, "s1", schema.UserMessage(txt)); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Load(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	// window=2:只唤起最近 2 条,文件保留全量
	if len(msgs) != 2 || msgs[0].Content != "b" || msgs[1].Content != "c" {
		t.Fatalf("window trim failed: %+v", msgs)
	}
	if err := s.Clear(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if msgs, _ = s.Load(ctx, "s1"); len(msgs) != 0 {
		t.Fatal("clear failed")
	}
}

// TestFileStorePathCollision 验证 sanitize 改写过的会话 ID 带哈希后缀,
// "a/b" 与 "a_b" 不再串线;本来安全的 ID 保持原名(兼容旧文件)。
func TestFileStorePathCollision(t *testing.T) {
	st, err := New(t.TempDir(), 0)
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
	fs := st.(*store)
	if got := fs.path("cli-session"); got != fs.dir+"/cli-session.jsonl" {
		t.Fatalf("safe id should keep plain filename, got %s", got)
	}
}
