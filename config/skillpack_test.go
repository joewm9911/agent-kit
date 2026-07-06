package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/skill"
)

var packModelOnce sync.Once

func registerPackTestModel() {
	packModelOnce.Do(func() {
		model.Register("packtest", func(context.Context, map[string]any) (einomodel.ToolCallingChatModel, error) {
			return testmodel.New(schema.AssistantMessage("[PACKOK]", nil)), nil
		})
	})
}

func writeConfigFixturePack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	md := "---\nname: report-writer\ndescription: 写一份结构化报告\n---\n你是报告写手,结论先行。"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBuildWithExternalSkillEntry:单文件 skills 列表混排外部引用——
// 装配期物化到 .skills + lock,产物以 kind=skillpack 进目录,可被调用。
func TestBuildWithExternalSkillEntry(t *testing.T) {
	registerPackTestModel()
	src := writeConfigFixturePack(t)
	root := t.TempDir()

	cfg := &Config{
		Profile: Profile{Model: &ModelConfig{Provider: "packtest"}},
		Skills: []*SkillEntry{
			{Use: "file:" + src},
		},
		Skillpacks: SkillpacksConfig{Dir: root},
	}
	app, err := Build(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := app.Catalog.Select([]string{"cap://skillpack/pack/report-writer"}, nil)
	if err != nil || len(caps) != 1 {
		t.Fatalf("skillpack not in catalog: %v %v", caps, err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills.lock")); err != nil {
		t.Fatalf("skills.lock not written: %v", err)
	}
	out, err := capability.Invoke(context.Background(), caps[0], `{"input":"写季报"}`)
	if err != nil || !strings.Contains(out, "[PACKOK]") {
		t.Fatalf("invoke: %q %v", out, err)
	}
}

// TestSkillEntryMutualExclusion:use 与内部声明字段互斥。
func TestSkillEntryMutualExclusion(t *testing.T) {
	registerPackTestModel()
	src := writeConfigFixturePack(t)
	cfg := &Config{
		Profile:    Profile{Model: &ModelConfig{Provider: "packtest"}},
		Skills:     []*SkillEntry{{Use: "file:" + src, Declaration: skill.Declaration{Engine: "react"}}},
		Skillpacks: SkillpacksConfig{Dir: t.TempDir()},
	}
	if _, err := Build(context.Background(), cfg, BuildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "互斥") {
		t.Fatalf("use+engine must fail fast, got %v", err)
	}
	// 平铺 skills 的 use 不接受内部引用形态
	cfg.Skills = []*SkillEntry{{Use: "components/foo"}}
	if _, err := Build(context.Background(), cfg, BuildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "外部链接") {
		t.Fatalf("internal use in flat skills must fail, got %v", err)
	}
}

// TestBuildRequireLocalMissing:require-local 下缺失即 fail fast。
func TestBuildRequireLocalMissing(t *testing.T) {
	registerPackTestModel()
	cfg := &Config{
		Profile: Profile{Model: &ModelConfig{Provider: "packtest"}},
		Skills: []*SkillEntry{{
			Use: "https://example.invalid/p.zip", Integrity: "sha256:" + strings.Repeat("a", 64),
		}},
		Skillpacks: SkillpacksConfig{Dir: t.TempDir(), Sync: "require-local"},
	}
	if _, err := Build(context.Background(), cfg, BuildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "require-local") {
		t.Fatalf("want require-local fail fast, got %v", err)
	}
}

// TestNamespaceExternalSkill:namespace 的 use: 外部链接——须显式 name,
// 产物 domain = namespace 名。
func TestNamespaceExternalSkill(t *testing.T) {
	src := writeConfigFixturePack(t)
	root := t.TempDir()
	global := source.NewCatalog(capability.RiskMutating, nil)

	ns := &NamespaceConfig{Name: "docs", Skills: []NamespaceSkill{
		{Name: "writer", Use: "file:" + src},
	}}
	err := buildNamespace(context.Background(), ns, nsDeps{
		global: global, prompts: (*prompt.Resolver)(nil),
		defaultModel: testmodel.New(schema.AssistantMessage("ok", nil)),
		maxRisk:      capability.RiskMutating,
		packRoot:     root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if caps, err := global.Select([]string{"cap://skillpack/docs/writer"}, nil); err != nil || len(caps) != 1 {
		t.Fatalf("namespace skillpack missing: %v %v", caps, err)
	}

	// 缺 name:fail fast
	ns2 := &NamespaceConfig{Name: "docs2", Skills: []NamespaceSkill{{Use: "file:" + src}}}
	err = buildNamespace(context.Background(), ns2, nsDeps{
		global:       source.NewCatalog(capability.RiskMutating, nil),
		defaultModel: testmodel.New(), maxRisk: capability.RiskMutating, packRoot: root,
	})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("namespace external without name must fail, got %v", err)
	}
}

// TestScriptPackAdmission:脚本型包风险 = Dangerous——默认目录准入拒收,
// 显式 catalog.max_risk: dangerous 才入目录(与 exectool 同一道闸)。
func TestScriptPackAdmission(t *testing.T) {
	registerPackTestModel()
	src := writeConfigFixturePack(t)
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "run.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := func(maxRisk string) *App {
		cfg := &Config{
			Profile:    Profile{Model: &ModelConfig{Provider: "packtest"}},
			Catalog:    CatalogConfig{MaxRisk: maxRisk},
			Skills:     []*SkillEntry{{Use: "file:" + src}},
			Skillpacks: SkillpacksConfig{Dir: t.TempDir()},
		}
		app, err := Build(context.Background(), cfg, BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	// 默认准入(mutating):Dangerous 包被拒收,选品必失败
	app := build("")
	if caps, _ := app.Catalog.Select([]string{"cap://skillpack/pack/report-writer"}, nil); len(caps) != 0 {
		t.Fatal("dangerous pack must be rejected by default admission")
	}
	// 显式提升:入目录
	app = build("dangerous")
	if caps, _ := app.Catalog.Select([]string{"cap://skillpack/pack/report-writer"}, nil); len(caps) != 1 {
		t.Fatal("dangerous pack should be admitted with max_risk: dangerous")
	}
}
