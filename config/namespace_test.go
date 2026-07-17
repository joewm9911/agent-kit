package config

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/runtime/loop"
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

// TestNamespaceAssembly:sources → skills(内联卡)→ subagents 全形态装配。
// 卡片与 sub-agent 进目录,卡片工具直挂,sub-agent 工具不可见。
func TestNamespaceAssembly(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New(schema.AssistantMessage("analyzed", nil))

	ns := &NamespaceConfig{
		Name:    "pipeline",
		Sources: []SourceConfig{{Name: "svc", Type: "nstest", Required: true}},
		Skills: []NamespaceSkill{{
			Name:        "quick-qa",
			Description: "快速问答",
			Params:      map[string]capability.ParamDecl{"q": {Type: "string", Required: true}},
			Prompt:      promptVal("按步骤:先 search 再总结。问题:{q}"),
			Tools:       []string{"tools/svc/search"},
		}},
		Subagents: []SubagentConfig{{
			Name:        "analyst",
			Description: "隔离分析",
			Params:      map[string]capability.ParamDecl{"request": {Type: "string", Required: true}},
			Prompt:      promptVal("根据请求 {request} 分析"),
			Tools:       []string{"tools/svc/submit"},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 目录内容:卡片 + 直挂工具 + sub-agent,恰好三项
	if metas := global.List(); len(metas) != 3 {
		t.Fatalf("catalog entries = %d, want 3 (card + mounted tool + subagent)", len(metas))
	}
	// 卡片:调用返回执行指引
	card, err := global.Get("cap://skill/pipeline/quick-qa")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), card, `{"q":"降噪耳机"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[过程卡|") || !strings.Contains(out, "降噪耳机") {
		t.Fatalf("card must return the rendered guide, got %q", out)
	}
	// 卡片工具直挂
	if _, err := global.Get("cap://tool/svc/search"); err != nil {
		t.Fatalf("card tools must mount to the host catalog: %v", err)
	}
	// sub-agent:身份 kind=agent,风险随工具传播(submit=mutating),端到端可执行
	sub, err := global.Get("cap://agent/pipeline/analyst")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Meta().Risk != capability.RiskMutating {
		t.Fatalf("risk propagation failed: %v", sub.Meta().Risk)
	}
	out, err = capability.Invoke(context.Background(), sub, `{"request":"发布服务"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "analyzed" {
		t.Fatalf("got %q", out)
	}
	// sub-agent 的工具不进目录(权限边界:submit 只在其内部工具面)
	if _, err := global.Get("cap://tool/svc/submit"); err == nil {
		t.Fatal("subagent tools must not mount to the host catalog")
	}
}

func TestNamespaceToolBoundary(t *testing.T) {
	setupTestSource()
	global := source.NewCatalog(capability.RiskMutating, nil)
	m := testmodel.New()

	// subagent 引用不存在于本 ns 的工具源 → 拒绝装配
	ns := &NamespaceConfig{
		Name:    "isolated",
		Sources: []SourceConfig{{Name: "svc", Type: "nstest"}},
		Subagents: []SubagentConfig{{
			Name:   "bad",
			Prompt: promptVal("x"),
			Tools:  []string{"tools/other/auth"},
		}},
	}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, defaultModel: m, maxRisk: capability.RiskMutating,
	})
	if err == nil || !strings.Contains(err.Error(), "matches no tool in namespace") {
		t.Fatalf("expect boundary error, got %v", err)
	}
}

// TestNamespaceRemovedKeys 覆盖概念收敛硬切:components/imports/steps/use/
// engine/deliver 误写全部装配期报错、文案指路。
func TestNamespaceRemovedKeys(t *testing.T) {
	setupTestSource()
	m := testmodel.New()
	build := func(ns *NamespaceConfig) error {
		return buildNamespace(context.Background(), ns, nsDeps{
			global: source.NewCatalog(capability.RiskMutating, nil), defaultModel: m, maxRisk: capability.RiskMutating,
		})
	}

	if err := build(&NamespaceConfig{Name: "a",
		ComponentsLegacy: []map[string]any{{"name": "x"}}}); err == nil ||
		!strings.Contains(err.Error(), "components has been removed") {
		t.Fatalf("components must fail with migration hint, got %v", err)
	}
	if err := build(&NamespaceConfig{Name: "b",
		ImportsLegacy: []string{"other"}}); err == nil ||
		!strings.Contains(err.Error(), "imports has been removed") {
		t.Fatalf("imports must fail with migration hint, got %v", err)
	}
	if err := build(&NamespaceConfig{Name: "c",
		Skills: []NamespaceSkill{{Name: "s", StepsLegacy: []map[string]any{{"name": "x"}}}}}); err == nil ||
		!strings.Contains(err.Error(), "steps has been removed") {
		t.Fatalf("steps must fail with migration hint, got %v", err)
	}
	use := "components/x"
	if err := build(&NamespaceConfig{Name: "d",
		Skills: []NamespaceSkill{{Name: "s", UseLegacy: &use}}}); err == nil ||
		!strings.Contains(err.Error(), "use has been removed") {
		t.Fatalf("use must fail with migration hint, got %v", err)
	}
	eng := "graph"
	if err := build(&NamespaceConfig{Name: "e",
		Skills: []NamespaceSkill{{Name: "s", EngineLegacy: &eng}}}); err == nil ||
		!strings.Contains(err.Error(), "engine has been removed") {
		t.Fatalf("engine must fail with migration hint, got %v", err)
	}
	deliver := "attach"
	if err := build(&NamespaceConfig{Name: "f",
		Skills: []NamespaceSkill{{Name: "s", DeliverLegacy: &deliver}}}); err == nil ||
		!strings.Contains(err.Error(), "sub-agent") {
		t.Fatalf("deliver on skill must point to subagents:, got %v", err)
	}
	if err := build(&NamespaceConfig{Name: "g",
		Subagents: []SubagentConfig{{Name: "x", Prompt: promptVal("p"),
			StepsLegacy: []map[string]any{{"name": "s"}}}}}); err == nil ||
		!strings.Contains(err.Error(), "steps/output has been removed") {
		t.Fatalf("subagent steps must fail, got %v", err)
	}
	export := true
	if err := build(&NamespaceConfig{Name: "h",
		Subagents: []SubagentConfig{{Name: "x", Prompt: promptVal("p"),
			ExportLegacy: &export}}}); err == nil ||
		!strings.Contains(err.Error(), "export has been removed") {
		t.Fatalf("subagent export must fail, got %v", err)
	}
}

// TestNamespaceCardOnlyKeys:内联卡不得声明 from 专属键(max_rounds/context)。
func TestNamespaceCardOnlyKeys(t *testing.T) {
	setupTestSource()
	m := testmodel.New()
	build := func(sk NamespaceSkill) error {
		return buildNamespace(context.Background(), &NamespaceConfig{Name: "z",
			Sources: []SourceConfig{{Name: "svc", Type: "nstest"}},
			Skills:  []NamespaceSkill{sk},
		}, nsDeps{global: source.NewCatalog(capability.RiskMutating, nil), defaultModel: m, maxRisk: capability.RiskMutating})
	}
	if err := build(NamespaceSkill{Name: "s", Prompt: promptVal("p"), MaxRounds: 5}); err == nil ||
		!strings.Contains(err.Error(), "max_rounds only applies to from") {
		t.Fatalf("card max_rounds must fail, got %v", err)
	}
	if err := build(NamespaceSkill{Name: "s", Prompt: promptVal("p"), Context: "fork"}); err == nil ||
		!strings.Contains(err.Error(), "context only applies to from") {
		t.Fatalf("card context must fail, got %v", err)
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

// TestInlineCardMountsTools:skills: 内联卡经多文件 app 路径装配,声明工具
// 直挂宿主目录("工具不出 ns"的唯一显式豁免)。
func TestInlineCardMountsTools(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: hi}}
agents: [agents/helper.yaml]
`,
		"agents/helper.yaml": `
description: 测试
namespaces: [../namespaces/inlinens.yaml]
`,
		"namespaces/inlinens.yaml": `
sources:
  - {name: svc, type: nstest}
skills:
  - name: quick-qa
    description: 快速问答过程卡
    prompt: "按步骤:先 search 再总结。问题:{$input}"
    tools: ["tools/svc/search"]
`,
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := BuildApp(context.Background(), spec, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mounted := app.AgentMounts["helper"]
	// 工具直挂:tools/svc/search 进了 agent 挂载目录
	if _, err := mounted.Get("cap://tool/svc/search"); err != nil {
		t.Fatalf("inline card tools must mount to the agent catalog: %v", err)
	}
	// 卡片可见,调用返回执行指引
	card, err := mounted.Get("cap://skill/inlinens/quick-qa")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), card, `{"input":"降噪耳机多少钱"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[过程卡|") || !strings.Contains(out, "先 search") {
		t.Fatalf("card must return the guide, got %q", out)
	}
}
