package loop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// TestProgressToolsEmitsMetaTruth:能力事件带 Ref.Kind/Domain 真值、
// 执行域与 builtin/custom 判定;start/done 成对,Detail 截断。
func TestProgressToolsEmitsMetaTruth(t *testing.T) {
	skill := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "skill", Domain: "catalog", Name: "check-inventory"},
	}, func(context.Context, string) (string, error) {
		return strings.Repeat("长", 300), nil
	})
	builtin := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "builtin", Name: "todo_write"},
	}, func(context.Context, string) (string, error) { return "ok", nil })

	caps := ProgressTools([]capability.Capability{skill, builtin})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []runctx.ProgressEvent
	ctx = runctx.WithProgress(ctx, func(_ context.Context, ev runctx.ProgressEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	ctx = runctx.WithScopePush(ctx, "comp:audit#1")

	if _, err := capability.Invoke(ctx, caps[0], `{"sku":"P103"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := capability.Invoke(ctx, caps[1], `{}`); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("events = %d, want 4(两能力各 start+done)", len(got))
	}
	s0, d0 := got[0], got[1]
	if s0.Status != "start" || s0.CapKind != "skill" || s0.Domain != "catalog" ||
		s0.Name != "check-inventory" || s0.ScopeKind != runctx.ScopeCustom || s0.Scope != "comp:audit#1" {
		t.Fatalf("skill start event wrong: %+v", s0)
	}
	if !strings.Contains(s0.Detail, "P103") {
		t.Fatalf("start detail should carry args: %q", s0.Detail)
	}
	if d0.Status != "done" || len([]rune(d0.Detail)) > progressDetailMax+3 {
		t.Fatalf("done event wrong or detail untruncated: %+v", d0)
	}
	if got[2].ScopeKind != runctx.ScopeBuiltin || got[2].Domain != "builtin" {
		t.Fatalf("builtin capability must be ScopeKind=builtin: %+v", got[2])
	}
}
