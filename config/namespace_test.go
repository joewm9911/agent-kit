package config

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/skill"
	"github.com/joewm9911/agent-kit/source"
)

func promptVal(s string) prompt.Value { return prompt.Value{Literal: s} }

var registerTestSource sync.Once

// testToolsSource 供给三个工具:auth、search、submit(mutating)。
func setupTestSource() {
	registerTestSource.Do(func() {
		source.Register("nstest", func(_ context.Context, name string, _ map[string]any) (source.Source, error) {
			mk := func(toolName string, risk capability.Risk, out string) capability.Capability {
				return capability.New(capability.Meta{
					Ref:         capability.Ref{Kind: "tool", Provider: "nstest", Namespace: name, Name: toolName},
					Description: toolName,
					Risk:        risk,
				}, func(_ context.Context, args string) (string, error) {
					return out + ":" + args, nil
				})
			}
			return source.Static(name,
				mk("auth", capability.RiskReadonly, "authed"),
				mk("search", capability.RiskReadonly, "found"),
				mk("submit", capability.RiskMutating, "submitted"),
			), nil
		})
	})
}

func TestNamespaceThreeLayerAssembly(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New(schema.AssistantMessage("plan-made", nil))

	ns := &NamespaceConfig{
		Name:  "pipeline",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest", Required: true}},
		Components: []ComponentConfig{{
			Name:   "planner",
			Prompt: promptVal("根据请求 {request} 制定计划"),
			Tools:  []string{"tools/svc/search"},
		}},
		Skills: []NamespaceSkill{{
			Name:        "deploy",
			Description: "鉴权 → 计划 → 提交",
			Params: map[string]skill.ParamDecl{
				"token":   {Type: "string", Required: true},
				"request": {Type: "string", Required: true},
			},
			Steps: []skill.Step{
				{Name: "auth", Use: "tools/svc/auth", Args: `{"token":"{token}"}`},
				{Name: "plan", Use: "components/planner", Args: `{"request":"{request}"}`},
				{Name: "run", Use: "tools/svc/submit", Args: `{"plan":"{plan}"}`},
			},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 只有 skill 进全局目录:component 与工具不可见
	metas := global.List()
	if len(metas) != 1 {
		t.Fatalf("global catalog should contain exactly the skill, got %d entries", len(metas))
	}
	if ref := metas[0].Ref.String(); ref != "cap://skill.graph/pipeline/deploy" {
		t.Fatalf("ref = %s", ref)
	}
	// 风险传播:submit 是 mutating → skill 也是
	if metas[0].Risk != capability.RiskMutating {
		t.Fatalf("risk = %v, want mutating", metas[0].Risk)
	}

	// 端到端执行:auth → planner(react,无工具调用退化单次)→ submit
	sk, err := global.Get("cap://skill.graph/pipeline/deploy")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{"token":"tk","request":"发布服务"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "submitted") || !strings.Contains(out, "plan-made") {
		t.Fatalf("got %q", out)
	}
}

func TestNamespaceToolBoundary(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New()

	// component 引用不存在于本 ns 的工具源 → 拒绝装配
	ns := &NamespaceConfig{
		Name:  "isolated",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Components: []ComponentConfig{{
			Name:   "bad",
			Prompt: promptVal("x"),
			Tools:  []string{"tools/other/auth"},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "no tool in this namespace") {
		t.Fatalf("expect boundary error, got %v", err)
	}
}

func TestNamespaceCrossRefOnlySkill(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New(schema.AssistantMessage("ok", nil))

	// ns1 导出一个 skill
	ns1 := &NamespaceConfig{
		Name:  "ns1",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Skills: []NamespaceSkill{{
			Name:  "lookup",
			Steps: []skill.Step{{Name: "s", Use: "tools/svc/search"}},
		}},
	}
	if err := buildNamespace(context.Background(), ns1, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	}); err != nil {
		t.Fatal(err)
	}

	// ns2 经 cap://skill 引用 ns1 的 skill:允许
	ns2 := &NamespaceConfig{
		Name: "ns2",
		Skills: []NamespaceSkill{{
			Name:  "wrap",
			Steps: []skill.Step{{Name: "s", Use: "cap://skill.graph/ns1/lookup"}},
		}},
	}
	if err := buildNamespace(context.Background(), ns2, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	}); err != nil {
		t.Fatal(err)
	}

	// ns3 试图经 cap://tool 引用工具:拒绝(工具不出命名空间)
	ns3 := &NamespaceConfig{
		Name: "ns3",
		Skills: []NamespaceSkill{{
			Name:  "steal",
			Steps: []skill.Step{{Name: "s", Use: "cap://tool.nstest/svc/search"}},
		}},
	}
	err := buildNamespace(context.Background(), ns3, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "only cap://skill") {
		t.Fatalf("expect cross-ns tool rejection, got %v", err)
	}
}
