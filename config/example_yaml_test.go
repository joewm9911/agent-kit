package config

import (
	"context"
	"testing"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/source"

	_ "github.com/joewm9911/agent-kit/provider/httptool"
)

// 校验示例配置:解析 + 两个命名空间完整装配(用假模型,不出网)。
func TestExampleYAMLNamespaces(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "dummy")
	cfg, err := Load("../examples/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Namespaces) != 2 {
		t.Fatalf("namespaces = %d", len(cfg.Namespaces))
	}
	if len(cfg.Skills) != 0 || len(cfg.Workflows) != 0 {
		t.Fatalf("flat skills/workflows should be gone: %d %d", len(cfg.Skills), len(cfg.Workflows))
	}

	prompts := prompt.NewResolver("")
	p, err := prompt.NewProvider("file", map[string]any{"dir": "../examples/prompts"})
	if err != nil {
		t.Fatal(err)
	}
	prompts.Add("pp", p)

	global := source.NewCatalog(capability.RiskMutating, nil)
	for i := range cfg.Namespaces {
		if err := buildNamespace(context.Background(), &cfg.Namespaces[i], nsDeps{
			global: global, prompts: prompts, defaultModel: testmodel.New(),
			maxRisk: capability.RiskMutating,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 目录里只有两个导出的 skill
	metas := global.List()
	if len(metas) != 2 {
		for _, m := range metas {
			t.Log(m.Ref.String())
		}
		t.Fatalf("catalog entries = %d, want 2", len(metas))
	}
	for _, ref := range []string{
		"cap://skill.graph/briefing/daily-brief",
		"cap://skill.graph/research/competitor_report@1",
	} {
		if _, err := global.Get(ref); err != nil {
			t.Fatalf("missing %s: %v", ref, err)
		}
	}
}
