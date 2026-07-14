package skill

// mode: inline(过程卡)装配与调用行为(single-agent-mode-plan 批1)。

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/prompt"
)

func TestInlineCardBuildAndInvoke(t *testing.T) {
	sk, err := Build(context.Background(), &Declaration{
		Name:        "t/price_review", // 纯 prompt+tools:结构即过程卡(无 mode 键)
		Description: "定价审查",
		Params:      map[string]capability.ParamDecl{"sku": {Type: "string", Required: true}},
		Prompt:      prompt.Value{Literal: "审查 {sku}:先查详情,再查销量,输出毛利率。原始诉求:{$user_input}"},
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	meta := sk.Meta()
	if meta.Risk != capability.RiskReadonly {
		t.Fatalf("card must be readonly, got %v", meta.Risk)
	}
	if !strings.Contains(meta.Description, "执行指引") {
		t.Fatalf("description must carry the guide suffix, got %q", meta.Description)
	}
	found := false
	for _, tag := range meta.Tags {
		if tag == capability.TagProcedureCard {
			found = true
		}
	}
	if !found {
		t.Fatal("card must carry TagProcedureCard")
	}

	ctx := runctx.WithLoopInput(context.Background(), "看看P103还能不能涨价")
	out, err := capability.Invoke(ctx, sk, `{"sku":"P103"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[过程卡|price_review]", "审查 P103", "看看P103还能不能涨价", "执行指引(不是已完成的结果)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("card output missing %q, got %q", want, out)
		}
	}
	// 必填缺失以结果回传
	out, _ = capability.Invoke(ctx, sk, `{}`)
	if !strings.Contains(out, "missing required parameter") {
		t.Fatalf("missing param must be reported, got %q", out)
	}
}

// 互斥校验:inline 与子循环专属键同时出现必须装配期报错。
func TestInlineMutualExclusions(t *testing.T) {
	// 结构决定形态:带子循环键 = 子执行体(照常装配,不再是互斥错误);
	// mode 键本身已移除,误写必须报错指路。
	legacy := "subloop"
	d := &Declaration{Name: "t/x", ModeLegacy: &legacy, Prompt: prompt.Value{Literal: "p"}}
	if _, err := Build(context.Background(), d, Deps{}); err == nil || !strings.Contains(err.Error(), "mode has been removed") {
		t.Fatalf("legacy mode key must fail fast with migration hint, got %v", err)
	}
	// 带 deliver 的声明自动成为子执行体(engine 缺省 react),不报互斥
	d2 := &Declaration{Name: "t/y", Deliver: "attach", Prompt: prompt.Value{Literal: "p"}}
	if _, err := Build(context.Background(), d2, Deps{DefaultModel: &echoInputModel{}}); err != nil {
		t.Fatalf("deliver implies sub-executor, must assemble: %v", err)
	}
}
