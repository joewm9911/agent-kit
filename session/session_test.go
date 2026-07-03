package session

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestCustomStoreRegistration(t *testing.T) {
	Register("custom-test", func(conf map[string]any, window int) (Store, error) {
		if conf["dsn"] != "x" {
			t.Fatalf("config not passed: %v", conf)
		}
		return NewInMemory(window), nil
	})
	s, err := New("custom-test", map[string]any{"dsn": "x"}, 5)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	if _, err := New("nope", nil, 0); err == nil || !strings.Contains(err.Error(), "custom-test") {
		t.Fatalf("unknown type should list registered types, got %v", err)
	}
}

func TestFileStoreRoundtripAndWindow(t *testing.T) {
	dir := t.TempDir()
	s, err := New("file", map[string]any{"dir": dir}, 2)
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

func TestSearchRelevant(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("我们聊聊竞品定价策略的问题"),
		schema.AssistantMessage("Notion 的定价是每人每月 10 美元", nil),
		schema.UserMessage("今天天气不错"),
		schema.SystemMessage("竞品定价 system 消息不应命中"),
	}
	hits := SearchRelevant(msgs, "竞品的定价", 2)
	if len(hits) == 0 {
		t.Fatal("expect relevant hits")
	}
	for _, h := range hits {
		if strings.Contains(h, "天气") || strings.Contains(h, "system 消息") {
			t.Fatalf("irrelevant/system message recalled: %s", h)
		}
	}
}
