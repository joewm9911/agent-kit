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

// 校验示例配置(多文件形态):LoadApp 解析三层文件 + 两个命名空间
// 完整装配(用假模型,不出网;不跑 BuildApp 以避开 MCP 源与真实模型)。
func TestExampleAppLayout(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "dummy")
	spec, err := LoadApp("../examples/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Agents) != 1 || spec.Agents[0].Name != "assistant" {
		t.Fatalf("agents = %+v", spec.Agents)
	}
	as := spec.Agents[0]
	if len(as.Mounts) != 2 {
		t.Fatalf("assistant mounts = %d, want 2", len(as.Mounts))
	}
	if as.Mounts[0].Name != "briefing" || as.Mounts[1].Name != "research" {
		t.Fatalf("mounts = %s, %s", as.Mounts[0].Name, as.Mounts[1].Name)
	}
	// 治理配置收在 agent 文件里
	if as.Approval != "interactive" || as.Budget.MaxModelCalls != 40 {
		t.Fatalf("agent governance config: approval=%q budget=%+v", as.Approval, as.Budget)
	}

	// 两个命名空间可完整装配(镜像 buildAgentFromSpec 的实例化路径)
	prompts := prompt.NewResolver("")
	p, err := prompt.NewProvider("file", map[string]any{"dir": "../examples/prompts"})
	if err != nil {
		t.Fatal(err)
	}
	prompts.Add("pp", p)

	mounted := source.NewCatalog(capability.RiskMutating, nil)
	cache := newSourceCache()
	for _, ns := range as.Mounts {
		nsCopy := ns.NamespaceConfig
		err := buildNamespace(context.Background(), &nsCopy, nsDeps{
			global: mounted, prompts: prompts, defaultModel: testmodel.New(),
			maxRisk: capability.RiskMutating,
			nsPath:  ns.Path, srcCache: cache,
			defaults: as.Defaults.merge(ns.Defaults),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 挂载目录里只有两个导出的 skill,component 与工具不泄漏
	metas := mounted.List()
	if len(metas) != 2 {
		for _, m := range metas {
			t.Log(m.Ref.String())
		}
		t.Fatalf("mounted entries = %d, want 2", len(metas))
	}
	for _, ref := range []string{
		"cap://skill/briefing/daily-brief",
		"cap://skill/research/competitor_report@1",
	} {
		if _, err := mounted.Get(ref); err != nil {
			t.Fatalf("missing %s: %v", ref, err)
		}
	}
}
