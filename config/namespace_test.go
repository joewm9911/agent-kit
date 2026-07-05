package config

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/prompt"
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
					Ref:         capability.Ref{Kind: "tool", Domain: name, Name: toolName},
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
			Engine: "react",
			Prompt: promptVal("根据请求 {request} 制定计划"),
			Tools:  []string{"tools/svc/search"},
		}},
		Skills: []NamespaceSkill{{
			Name:        "deploy",
			Description: "鉴权 → 计划 → 提交",
			Params: map[string]capability.ParamDecl{
				"token":   {Type: "string", Required: true},
				"request": {Type: "string", Required: true},
			},
			Steps: []engine.Step{
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
	if ref := metas[0].Ref.String(); ref != "cap://skill/pipeline/deploy" {
		t.Fatalf("ref = %s", ref)
	}
	// 风险传播:submit 是 mutating → skill 也是
	if metas[0].Risk != capability.RiskMutating {
		t.Fatalf("risk = %v, want mutating", metas[0].Risk)
	}

	// 端到端执行:auth → planner(react,无工具调用退化单次)→ submit
	sk, err := global.Get("cap://skill/pipeline/deploy")
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
			Engine: "react",
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

// TestGraphComponentAndSkillUse 覆盖编排族 component(workflow/graph)
// 与 skill 的 use: 入口引用形态。
func TestGraphComponentAndSkillUse(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New(schema.AssistantMessage("summarized", nil))

	ns := &NamespaceConfig{
		Name:  "flows",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Components: []ComponentConfig{
			{
				// workflow 形态:顺序钉死的私有序列(工具 → 模型),模型只出现一次
				Name:   "lookup",
				Engine: "workflow",
				Params: map[string]capability.ParamDecl{"q": {Type: "string", Required: true}},
				Steps: []engine.Step{
					{Name: "data", Use: "tools/svc/search", Args: `{"q":"{q}"}`},
					{Name: "say", Use: "model", Args: "总结:{data}"},
				},
			},
			{
				// graph 形态:并行 fan-out + 汇合,引用前面的编排族 component
				Name:   "wide",
				Engine: "graph",
				Params: map[string]capability.ParamDecl{"q": {Type: "string"}},
				Steps: []engine.Step{
					{Name: "a", Use: "tools/svc/search", Needs: []string{}, Args: `{"q":"{q}"}`},
					{Name: "b", Use: "tools/svc/auth", Needs: []string{}, Args: `{"q":"{q}"}`},
					{Name: "join", Use: "components/lookup", Needs: []string{"a", "b"}, Args: `{"q":"{a}+{b}"}`},
				},
			},
		},
		Skills: []NamespaceSkill{{
			// use: 入口引用:skill 退化为纯接口,执行委托给 graph component
			Name:        "wide-search",
			Description: "并行检索并总结",
			Params:      map[string]capability.ParamDecl{"q": {Type: "string", Required: true}},
			Use:         "components/wide",
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 目录里只有导出的 skill,两个编排族 component 不可见
	if metas := global.List(); len(metas) != 1 {
		t.Fatalf("catalog entries = %d, want 1", len(metas))
	}
	sk, err := global.Get("cap://skill/flows/wide-search")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{"q":"pay"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "summarized" {
		t.Fatalf("got %q", out)
	}
}

func TestWorkflowComponentRejectsNeeds(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	ns := &NamespaceConfig{
		Name:  "badflow",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Components: []ComponentConfig{{
			Name:   "x",
			Engine: "workflow",
			Steps: []engine.Step{
				{Name: "a", Use: "tools/svc/search"},
				{Name: "b", Use: "tools/svc/search", Needs: []string{"a"}},
			},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: testmodel.New(), maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "不支持 needs") {
		t.Fatalf("expect workflow needs rejection, got %v", err)
	}
}

func TestGraphComponentMutuallyExclusive(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	ns := &NamespaceConfig{
		Name:  "mixed",
		Tools: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Components: []ComponentConfig{{
			Name:   "bad",
			Engine: "react",
			Prompt: promptVal("x"),
			Steps:  []engine.Step{{Name: "s", Use: "tools/svc/search"}},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: testmodel.New(), maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("expect mutual-exclusion error, got %v", err)
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
			Steps: []engine.Step{{Name: "s", Use: "tools/svc/search"}},
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
			Steps: []engine.Step{{Name: "s", Use: "cap://skill/ns1/lookup"}},
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
			Steps: []engine.Step{{Name: "s", Use: "cap://tool/svc/search"}},
		}},
	}
	err := buildNamespace(context.Background(), ns3, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "only cap://skill") {
		t.Fatalf("expect cross-ns tool rejection, got %v", err)
	}
}

// TestWindowMustFitSummaryView 验证装配期校验:窗口容不下摘要视图
// (摘要+锚定+保留消息)时拒绝装配,不让滚动记忆被静默裁掉。
func TestWindowMustFitSummaryView(t *testing.T) {
	setupTestSource()
	cfg := &Config{
		Agents: []AgentConfig{{Name: "bad"}},
	}
	cfg.Agents[0].Session.Window = 8
	cfg.Agents[0].Loop.Compaction = &loop.CompactionConfig{MaxMessages: 30, KeepRecent: 10}
	cfg.Agents[0].Model = &ModelConfig{Provider: "marker", Config: map[string]any{"resp": "x"}}
	setupAppTestFakes() // 注册 marker 模型
	_, err := Build(context.Background(), cfg, BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "keep_recent") {
		t.Fatalf("expect window/keep validation error, got %v", err)
	}
}
