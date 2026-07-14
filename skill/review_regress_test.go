package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/prompt"
)

// TestPersonaNotLeakedOnEmptyInput(M1 回归):嵌套调用里上游 input 渲染为空
// 时组件走降级(prompt→用户消息),此时必须清空 ctx 里外层组件的 persona——
// 否则内层顶着外层身份跑(系统消息里出现外层 persona)。
func TestPersonaNotLeakedOnEmptyInput(t *testing.T) {
	em := &echoInputModel{}
	comp, err := Build(context.Background(), &Declaration{
		Engine: "react", // 结构决定形态:声明 engine = 子执行体(mode 已移除)
		Name:   "t/inner",
		Prompt: prompt.Value{Literal: "你是内层组件"},
	}, Deps{DefaultModel: em})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟:外层组件已设 persona,随后作用域输入被重设为空(上游步骤输出为空)
	ctx := runctx.WithPersona(context.Background(), "你是外层财务专员(不该出现)")
	ctx = runctx.WithInput(ctx, "")
	if _, err := capability.Invoke(ctx, comp, `{}`); err != nil {
		t.Fatal(err)
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	for _, call := range em.seen {
		for _, msg := range call {
			if msg.Role == schema.System && strings.Contains(msg.Content, "外层财务专员") {
				t.Fatalf("outer persona leaked into inner component system message:\n%s", msg.Content)
			}
		}
	}
	// 降级语义不变:内层 prompt 作为用户消息到场
	found := false
	for _, call := range em.seen {
		for _, msg := range call {
			if msg.Role == schema.User && strings.Contains(msg.Content, "你是内层组件") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("degraded prompt-as-user-message missing")
	}
}

// TestLoopFamilyRequiredParams(M2 回归):循环族组件缺必填参数时,与 graph
// 族同语义——以结果回传"call not executed: missing required parameter(s)",
// 让上级大脑补参重试,而不是把 {placeholder} 字面量留在 persona 里。
func TestLoopFamilyRequiredParams(t *testing.T) {
	em := &echoInputModel{}
	comp, err := Build(context.Background(), &Declaration{
		Engine: "react", // 结构决定形态:声明 engine = 子执行体(mode 已移除)
		Name:   "t/strict",
		Params: map[string]capability.ParamDecl{"category": {Type: "string", Required: true}},
		Prompt: prompt.Value{Literal: "盘点品类「{category}」"},
	}, Deps{DefaultModel: em})
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), comp, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "missing required parameter(s) category") {
		t.Fatalf("missing required param must return self-correcting message, got %q", out)
	}
	if len(em.seen) != 0 {
		t.Fatal("model must not be called when required params are missing")
	}
	// 参数给齐则正常执行
	if out, err = capability.Invoke(context.Background(), comp, `{"category":"音频"}`); err != nil || out != "done" {
		t.Fatalf("normal path broken: %q %v", out, err)
	}
}
