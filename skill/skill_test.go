package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/source"
)

func testCatalog(t *testing.T) *source.Catalog {
	t.Helper()
	c := source.NewCatalog(capability.RiskMutating, nil)
	write := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "fs", Name: "write_file"},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) { return "written", nil })
	if err := c.Add(write); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSkillBuildAndInvoke(t *testing.T) {
	m := testmodel.New(
		testmodel.ToolCallMsg("write_file", `{"input":"x"}`),
		schema.AssistantMessage("报告完成", nil),
	)
	decl := &Declaration{
		Name:        "research/report",
		Version:     "1",
		Description: "生成报告",
		Params:      map[string]capability.ParamDecl{"topic": {Type: "string", Required: true}},
		Prompt:      prompt.Value{Literal: "为 {topic} 生成报告"},
		Capabilities: struct {
			Include []string `yaml:"include"`
			Exclude []string `yaml:"exclude"`
		}{Include: []string{"cap://tool/fs/write_file"}},
	}
	cap_, err := Build(context.Background(), decl, Deps{Catalog: testCatalog(t), DefaultModel: m})
	if err != nil {
		t.Fatal(err)
	}

	meta := cap_.Meta()
	if meta.Ref.String() != "cap://skill/research/report@1" {
		t.Fatalf("ref = %s", meta.Ref)
	}
	// 风险传播:绑定了 mutating 工具,skill 整体应为 mutating
	if meta.Risk != capability.RiskMutating {
		t.Fatalf("risk propagation failed: %v", meta.Risk)
	}

	out, err := capability.Invoke(context.Background(), cap_, `{"topic":"Notion"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "报告完成" {
		t.Fatalf("got %q", out)
	}
}

func TestSkillDependencyCheck(t *testing.T) {
	decl := &Declaration{
		Name:   "x/y",
		Prompt: prompt.Value{Literal: "do {input}"},
		Capabilities: struct {
			Include []string `yaml:"include"`
			Exclude []string `yaml:"exclude"`
		}{Include: []string{"cap://tool/fs/not_exist"}},
	}
	_, err := Build(context.Background(), decl, Deps{Catalog: testCatalog(t), DefaultModel: testmodel.New()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expect dependency error, got %v", err)
	}
}
