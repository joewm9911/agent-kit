package session

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// nopStore 是最小 Store,仅供注册表测试(具体后端在 impl/session/* 各自测)。
type nopStore struct{}

func (nopStore) Load(context.Context, string) ([]*schema.Message, error)  { return nil, nil }
func (nopStore) Append(context.Context, string, ...*schema.Message) error { return nil }
func (nopStore) Clear(context.Context, string) error                      { return nil }

func TestCustomStoreRegistration(t *testing.T) {
	Register("custom-test", func(conf map[string]any, window int) (Store, error) {
		if conf["dsn"] != "x" {
			t.Fatalf("config not passed: %v", conf)
		}
		return nopStore{}, nil
	})
	s, err := New("custom-test", map[string]any{"dsn": "x"}, 5)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	// 未注册类型:fail-fast 且列出已注册项
	if _, err := New("nope", nil, 0); err == nil || !strings.Contains(err.Error(), "custom-test") {
		t.Fatalf("unknown type should list registered types, got %v", err)
	}
}

func TestTrim(t *testing.T) {
	msgs := []*schema.Message{schema.UserMessage("a"), schema.UserMessage("b"), schema.UserMessage("c")}
	if got := Trim(msgs, 2); len(got) != 2 || got[0].Content != "b" {
		t.Fatalf("trim window=2: %+v", got)
	}
	if got := Trim(msgs, 0); len(got) != 3 {
		t.Fatalf("trim window<=0 should not cut: %+v", got)
	}
}
