package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
)

// frontPackDir 造一个带扩展 frontmatter 的纯指令技能包目录。
func frontPackDir(t *testing.T, front string) string {
	t.Helper()
	dir := t.TempDir()
	md := "---\nname: probe\ndescription: 探针技能\n" + front + "\n---\n[HUBBODY] 按指引行事。"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hubTestConfig(t *testing.T, front string) *Config {
	t.Helper()
	return &Config{
		Profile:    Profile{Model: &ModelConfig{Provider: "packtest"}},
		Skills:     []*SkillEntry{{Use: "file:" + frontPackDir(t, front)}},
		Skillpacks: SkillpacksConfig{Dir: t.TempDir()},
	}
}

// TestSkillpackModelHubWiring 验证 frontmatter model: 走顶层 models: 具名
// 模型;未声明 fail fast。
func TestSkillpackModelHubWiring(t *testing.T) {
	registerPackTestModel()

	cfg := hubTestConfig(t, "model: fast")
	cfg.Models = []NamedModelConfig{{Name: "fast", Provider: "packtest"}}
	app, err := Build(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.Catalog.Get("cap://skill/pack/probe")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), c, `{"input":"干活"}`)
	if err != nil || !strings.Contains(out, "[PACKOK]") {
		t.Fatalf("skill via named model: %q %v", out, err)
	}

	// 未声明的具名模型:装配 fail fast
	cfg2 := hubTestConfig(t, "model: ghost")
	if _, err := Build(context.Background(), cfg2, BuildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "未声明") {
		t.Fatalf("unknown named model must fail fast, got %v", err)
	}
}

// TestSkillpackAgentHubWiring 验证 frontmatter agent::装配期校验已声明
// agent 名;调用期委托给已装配 agent(端到端穿 invokeAsSub)。
func TestSkillpackAgentHubWiring(t *testing.T) {
	registerPackTestModel()

	cfg := hubTestConfig(t, "agent: helper")
	cfg.Agents = []AgentConfig{{Name: "helper", Description: "帮手"}}
	app, err := Build(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.Catalog.Get("cap://skill/pack/probe")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), c, `{"input":"查P100"}`)
	if err != nil || !strings.Contains(out, "[PACKOK]") {
		t.Fatalf("delegate through assembled agent: %q %v", out, err)
	}

	// 未声明的 agent 名:装配 fail fast
	cfg2 := hubTestConfig(t, "agent: ghost")
	cfg2.Agents = []AgentConfig{{Name: "helper"}}
	if _, err := Build(context.Background(), cfg2, BuildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "未在本 app 声明") {
		t.Fatalf("unknown agent must fail fast at assembly, got %v", err)
	}
}
