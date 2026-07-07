package config

import (
	"context"
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
		{Name: "fetch", Use: "tools/cms/get_doc", Args: prompt.Value{Literal: `{"doc_id":"p1"}`}},
		{Name: "answer", Use: "model", Args: prompt.Value{Ref: "cap://prompt/pp/policy-answer"}},
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
