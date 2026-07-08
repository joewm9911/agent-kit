package config

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
)

type fakePromptProvider struct{ tpl *prompt.Template }

func (f fakePromptProvider) Get(context.Context, string, string) (*prompt.Template, error) {
	return f.tpl, nil
}

// TestModelStepPromptArgsSplit:提示词与参数彻底拆分——
// model 步骤 prompt 必填(前缀识别引用,装配期锁版本),args 是纯参数
// 映射(绑定进模板占位符,键不存在报错);工具步骤不得有 prompt,
// args 映射拼为 JSON 对象模板。
func TestModelStepPromptArgsSplit(t *testing.T) {
	var st struct {
		Steps []engine.Step `yaml:"steps"`
	}
	doc := `
steps:
  - name: fetch
    use: "tools/cms/get_doc"
    args: {doc_id: policy-2026, version: latest}
  - name: answer
    use: model
    prompt: "cap://prompt/pp/mask"
    args: {analysis: "{fetch}", tone: formal}
  - name: plain
    use: model
    prompt: "字面量提示词:{fetch} / {$input}"
`
	if err := yaml.Unmarshal([]byte(doc), &st); err != nil {
		t.Fatal(err)
	}
	if st.Steps[0].Args.Fields["doc_id"] != "policy-2026" {
		t.Fatalf("tool args mapping: %+v", st.Steps[0].Args)
	}
	if st.Steps[1].Prompt.Ref != "cap://prompt/pp/mask" || st.Steps[1].Args.Fields["tone"] != "formal" {
		t.Fatalf("model step parse: prompt=%+v args=%+v", st.Steps[1].Prompt, st.Steps[1].Args)
	}
	if st.Steps[2].Prompt.Literal == "" {
		t.Fatalf("literal prompt: %+v", st.Steps[2].Prompt)
	}

	r := prompt.NewResolver("")
	r.Add("pp", fakePromptProvider{&prompt.Template{Name: "mask", Version: "v1",
		Text: "分析 {analysis},口吻 {tone},问题 {$input}"}})
	out, err := resolveStepArgs(context.Background(), st.Steps, r)
	if err != nil {
		t.Fatal(err)
	}
	// 工具步骤:参数映射拼为 JSON 对象模板
	if lit := out[0].Args.Literal; !strings.Contains(lit, `"doc_id":"policy-2026"`) {
		t.Fatalf("tool args json: %q", lit)
	}
	// model 步骤:引用解析 + 绑定代入,收敛为字面量;prompt 已消费
	if got := out[1].Args.Literal; got != "分析 {fetch},口吻 formal,问题 {$input}" {
		t.Fatalf("model prompt spliced: %q", got)
	}
	if !out[1].Prompt.IsZero() || out[1].Args.Fields != nil {
		t.Fatalf("prompt/fields should be consumed: %+v %+v", out[1].Prompt, out[1].Args)
	}
	if out[2].Args.Literal != "字面量提示词:{fetch} / {$input}" {
		t.Fatalf("literal prompt converged: %q", out[2].Args.Literal)
	}
}

// TestStepPromptArgsFailFast:拆分后的全部违规形态装配期/解析期报错。
func TestStepPromptArgsFailFast(t *testing.T) {
	r := prompt.NewResolver("")
	r.Add("pp", fakePromptProvider{&prompt.Template{Text: "模板 {x}"}})

	// model 缺 prompt
	if _, err := resolveStepArgs(context.Background(), []engine.Step{
		{Name: "m", Use: "model"}}, r); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("model without prompt must fail, got %v", err)
	}
	// 工具步骤带 prompt
	if _, err := resolveStepArgs(context.Background(), []engine.Step{
		{Name: "t", Use: "tools/a/b", Prompt: prompt.Value{Literal: "x"}}}, r); err == nil ||
		!strings.Contains(err.Error(), "use: model") {
		t.Fatalf("prompt on tool step must fail, got %v", err)
	}
	// 无效参数键:模板没有对应占位符
	if _, err := resolveStepArgs(context.Background(), []engine.Step{
		{Name: "m", Use: "model", Prompt: prompt.Value{Ref: "cap://prompt/pp/t"},
			Args: engine.StepArgs{Fields: map[string]string{"nope": "1"}}}}, r); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("invalid bind key must fail, got %v", err)
	}
	// model 步骤 args 写了标量(提示词误入 args)
	if _, err := resolveStepArgs(context.Background(), []engine.Step{
		{Name: "m", Use: "model", Prompt: prompt.Value{Literal: "p"},
			Args: engine.StepArgs{Literal: "误当提示词"}}}, r); err == nil ||
		!strings.Contains(err.Error(), "parameter mapping") {
		t.Fatalf("scalar args on model step must fail, got %v", err)
	}

	// 解析期:args 标量写提示词引用 → 指路 prompt:
	var bad struct {
		Steps []engine.Step `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte("steps:\n  - {name: x, use: model, args: \"cap://prompt/pp/t\"}\n"), &bad); err == nil ||
		!strings.Contains(err.Error(), "prompt:") {
		t.Fatalf("prompt ref in args must fail with hint, got %v", err)
	}
	// 解析期:args 映射带 use/ref 保留键 → 指路 prompt:
	if err := yaml.Unmarshal([]byte("steps:\n  - {name: x, use: model, args: {use: \"cap://prompt/pp/t\"}}\n"), &bad); err == nil ||
		!strings.Contains(err.Error(), "prompt:") {
		t.Fatalf("legacy use key must fail with hint, got %v", err)
	}
}
