package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	agexec "github.com/joewm9911/agent-kit/protocol/exec"
	"github.com/joewm9911/agent-kit/protocol/source"
)

var cfgSandboxOnce sync.Once

func registerCfgFakeSandbox() {
	cfgSandboxOnce.Do(func() {
		agexec.RegisterSandbox("cfgfake", func(map[string]any) (agexec.Sandbox, error) {
			return cfgFakeSandbox{}, nil
		})
	})
}

type cfgFakeSandbox struct{}

func (cfgFakeSandbox) Exec(_ context.Context, script string, _ []string) (string, error) {
	return "[CFGFAKE]" + script, nil
}

// scriptPackDir 造一个带 .py 脚本的技能包(→ runtimes=[python] → 需要 exec 工具)。
func scriptPackDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	md := "---\nname: tooler\ndescription: 带脚本的技能\n---\n用 python 工具干活。"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSkillpackExecPropagation 验证批D:app 级 exec 策略透传到 pack 的 exec
// 工具——require_sandbox 且无默认沙箱时,脚本包装配 fail fast;配上
// default_sandbox 后放行。直接证明 execCfg 抵达了 pack 的 exec 构造。
func TestSkillpackExecPropagation(t *testing.T) {
	registerPackTestModel()
	registerCfgFakeSandbox()
	src := scriptPackDir(t)

	build := func(exec ExecConfig) error {
		cfg := &Config{
			Profile:    Profile{Model: &ModelConfig{Provider: "packtest"}},
			Catalog:    CatalogConfig{MaxRisk: "dangerous"},
			Skills:     []*SkillEntry{{Use: "file:" + src}},
			Skillpacks: SkillpacksConfig{Dir: t.TempDir()},
			Exec:       exec,
		}
		_, err := Build(context.Background(), cfg, BuildOptions{})
		return err
	}

	// require_sandbox 且无默认沙箱:pack 脚本无沙箱可用 → fail fast
	if err := build(ExecConfig{RequireSandbox: true}); err == nil ||
		!strings.Contains(err.Error(), "require_sandbox") {
		t.Fatalf("require_sandbox without default must fail fast, got %v", err)
	}

	// 配上 default_sandbox:透传到 pack 的 exec 工具,满足 require → 放行
	if err := build(ExecConfig{DefaultSandbox: "cfgfake", RequireSandbox: true}); err != nil {
		t.Fatalf("default_sandbox should satisfy require_sandbox for pack scripts: %v", err)
	}
}

// TestNamespaceExecInjection 验证 app 级 exec 策略同样覆盖 namespace 内声明的
// exec 源(顶层 sources、skillpack、ns tools 三条装配路径口径一致)。
func TestNamespaceExecInjection(t *testing.T) {
	registerCfgFakeSandbox()

	ns := func() *NamespaceConfig {
		return &NamespaceConfig{
			Name: "calc",
			Tools: []SourceConfig{{
				Name: "py", Type: "exec", Required: true,
				Config: map[string]any{
					"tools": []any{map[string]any{"name": "python", "runtime": "python"}},
				},
			}},
		}
	}
	deps := func(exec ExecConfig) nsDeps {
		return nsDeps{
			global:  source.NewCatalog(capability.RiskDangerous, nil),
			maxRisk: capability.RiskDangerous,
			execCfg: exec,
			nsPath:  t.Name(), // srcCache 键按 (ns 文件, 源名),测试间隔离
		}
	}

	// require_sandbox 且无默认沙箱 → ns 内 exec 工具装配 fail fast
	err := buildNamespace(context.Background(), ns(), deps(ExecConfig{RequireSandbox: true}))
	if err == nil || !strings.Contains(err.Error(), "require_sandbox") {
		t.Fatalf("namespace exec tool must fail fast under require_sandbox, got %v", err)
	}

	// 配上 default_sandbox → 注入 ns 的 exec 源,放行
	if err := buildNamespace(context.Background(), ns(),
		deps(ExecConfig{DefaultSandbox: "cfgfake", RequireSandbox: true})); err != nil {
		t.Fatalf("default_sandbox should reach namespace exec source: %v", err)
	}
}
