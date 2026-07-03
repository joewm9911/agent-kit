package source

import (
	"context"
	"strings"
	"testing"

	"github.com/cloverzhang/agent-kit/capability"
)

func mkCap(ns, name string, risk capability.Risk) capability.Capability {
	return capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Provider: "test", Namespace: ns, Name: name},
		Risk: risk,
	}, func(ctx context.Context, _ string) (string, error) { return ns + "/" + name, nil })
}

func TestCatalogAdmissionAndSelect(t *testing.T) {
	c := NewCatalog(capability.RiskMutating, nil)
	err := c.AddSource(context.Background(), Static("fs",
		mkCap("fs", "read", capability.RiskReadonly),
		mkCap("fs", "write", capability.RiskMutating),
		mkCap("fs", "rm_rf", capability.RiskDangerous), // 超过准入上限,应被拒
	), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.List()); got != 2 {
		t.Fatalf("dangerous capability should be rejected, got %d entries", got)
	}

	caps, err := c.Select([]string{"cap://tool.test/fs/*"}, []string{"cap://tool.test/fs/write"})
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 || caps[0].Meta().Ref.Name != "read" {
		t.Fatalf("exclude failed: %+v", caps)
	}
}

func TestCatalogConflictAndPriority(t *testing.T) {
	c := NewCatalog(capability.RiskMutating, nil)
	ctx := context.Background()
	if err := c.AddSource(ctx, Static("a", mkCap("a", "search", 0)), true, 0); err != nil {
		t.Fatal(err)
	}
	// 同 Key 同优先级 → 报错
	if err := c.AddSource(ctx, Static("a", mkCap("a", "search", 0)), true, 0); err == nil {
		t.Fatal("expect conflict error for equal priority duplicate")
	}
	// 高优先级 → 遮蔽
	if err := c.AddSource(ctx, Static("a", mkCap("a", "search", 0)), true, 1); err != nil {
		t.Fatalf("higher priority should shadow: %v", err)
	}
}

func TestCatalogAliasUpgrade(t *testing.T) {
	c := NewCatalog(capability.RiskMutating, nil)
	ctx := context.Background()
	_ = c.AddSource(ctx, Static("fs", mkCap("fs", "search", 0)), true, 0)
	_ = c.AddSource(ctx, Static("jira", mkCap("jira", "search", 0)), true, 0)

	caps, err := c.Select([]string{"cap://tool.test/*/*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 2 {
		t.Fatalf("want 2 caps, got %d", len(caps))
	}
	names := map[string]bool{}
	for _, cp := range caps {
		tl, err := cp.AsTool(ctx)
		if err != nil {
			t.Fatal(err)
		}
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	if !names["fs_search"] || !names["jira_search"] {
		t.Fatalf("alias upgrade failed, got %v", names)
	}
}

func TestOptionalSourceDegrade(t *testing.T) {
	c := NewCatalog(capability.RiskMutating, nil)
	bad := &failSource{}
	if err := c.AddSource(context.Background(), bad, false, 0); err != nil {
		t.Fatalf("optional source failure should degrade, got %v", err)
	}
	if err := c.AddSource(context.Background(), bad, true, 0); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("required source failure should error, got %v", err)
	}
}

type failSource struct{}

func (f *failSource) Name() string { return "bad" }
func (f *failSource) Sync(context.Context) ([]capability.Capability, error) {
	return nil, context.DeadlineExceeded
}
