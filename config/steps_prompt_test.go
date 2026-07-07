package config

import (
	"context"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
)

type fakePromptProvider struct{ tpl *prompt.Template }

func (f fakePromptProvider) Get(context.Context, string, string) (*prompt.Template, error) {
	return f.tpl, nil
}

// TestResolveStepArgsPromptRef:编排步 args 的 {ref: cap://prompt/...}
// 在装配期解析为平台模板体(锁版本),字面量步骤原样透传;未配提示词
// 源时引用步骤装配期报错(fail fast)。
func TestResolveStepArgsPromptRef(t *testing.T) {
	r := prompt.NewResolver("")
	r.Add("pp", fakePromptProvider{&prompt.Template{
		Name: "policy-answer", Version: "v7", Text: "依据 {fetch} 回答:{$input}",
	}})
	steps := []engine.Step{
		{Name: "fetch", Use: "tools/cms/get_doc", Args: engine.StepArgs{Value: prompt.Value{Literal: `{"doc_id":"p1"}`}}},
		{Name: "answer", Use: "model", Args: engine.StepArgs{Value: prompt.Value{Ref: "cap://prompt/pp/policy-answer"}}},
	}
	out, err := resolveStepArgs(context.Background(), steps, r)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Args.Literal != `{"doc_id":"p1"}` {
		t.Fatalf("literal step mutated: %+v", out[0].Args)
	}
	if out[1].Args.Ref != "" || out[1].Args.Literal != "依据 {fetch} 回答:{$input}" {
		t.Fatalf("ref not resolved to template text: %+v", out[1].Args)
	}

	// 未配提示词源:引用必须在装配期报错,不留运行期惊喜
	if _, err := resolveStepArgs(context.Background(), steps, nil); err == nil ||
		!strings.Contains(err.Error(), "no prompt sources") {
		t.Fatalf("nil resolver must fail fast, got %v", err)
	}
}

// TestStepArgsBindings:args 映射形态 = ref + 使用点绑定(ref 之外的键
// 即绑定,不再有键被静默丢弃):绑定代入模板、无效绑定键装配期报错、
// 缺 ref 的映射解析期报错、标量写法不受影响。
func TestStepArgsBindings(t *testing.T) {
	// yaml 两种写法
	var st struct {
		Steps []engine.Step `yaml:"steps"`
	}
	doc := `
steps:
  - name: a
    use: model
    args: "字面量 {x}"
  - name: b
    use: model
    args:
      ref: "cap://prompt/pp/mask"
      analysis: "{a}"
      tone: formal
`
	if err := yaml.Unmarshal([]byte(doc), &st); err != nil {
		t.Fatal(err)
	}
	if st.Steps[0].Args.Literal != "字面量 {x}" || st.Steps[0].Args.Binds != nil {
		t.Fatalf("scalar form: %+v", st.Steps[0].Args)
	}
	if st.Steps[1].Args.Ref != "cap://prompt/pp/mask" ||
		st.Steps[1].Args.Binds["analysis"] != "{a}" || st.Steps[1].Args.Binds["tone"] != "formal" {
		t.Fatalf("mapping form: %+v", st.Steps[1].Args)
	}

	// 缺 ref 的映射:解析期报错
	var bad struct {
		Steps []engine.Step `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte("steps:\n  - name: x\n    use: model\n    args: {analysis: \"{a}\"}\n"), &bad); err == nil ||
		!strings.Contains(err.Error(), "ref") {
		t.Fatalf("mapping without ref must fail, got %v", err)
	}

	// 装配:绑定代入模板,剩余占位符留给运行时
	r := prompt.NewResolver("")
	r.Add("pp", fakePromptProvider{&prompt.Template{Name: "mask", Version: "v1",
		Text: "分析 {analysis},口吻 {tone},问题 {$input}"}})
	out, err := resolveStepArgs(context.Background(), []engine.Step{st.Steps[1]}, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].Args.Literal; got != "分析 {a},口吻 formal,问题 {$input}" {
		t.Fatalf("bindings not spliced: %q", got)
	}
	if out[0].Args.Binds != nil && len(out[0].Args.Binds) != 0 {
		// 绑定已消费(Literal 化后 Binds 不再携带)
		t.Fatalf("binds should be consumed: %+v", out[0].Args.Binds)
	}

	// 无效绑定键:模板里没有对应占位符 → 装配期报错
	badBind := st.Steps[1]
	badBind.Args.Binds = map[string]string{"nope": "x"}
	if _, err := resolveStepArgs(context.Background(), []engine.Step{badBind}, r); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("invalid bind key must fail, got %v", err)
	}
}
