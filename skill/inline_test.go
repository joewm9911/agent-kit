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
		Name:        "t/price_review",
		Mode:        "inline",
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
	base := func() *Declaration {
		return &Declaration{Name: "t/x", Mode: "inline", Prompt: prompt.Value{Literal: "p"}}
	}
	cases := []struct {
		name string
		mut  func(*Declaration)
		want string
	}{
		{"engine", func(d *Declaration) { d.Engine = "react" }, "engine"},
		{"deliver", func(d *Declaration) { d.Deliver = "attach" }, "deliver"},
		{"todo", func(d *Declaration) { d.Todo = true }, "todo"},
		{"model", func(d *Declaration) { d.Model = &ModelDecl{Provider: "x"} }, "model"},
		{"max_rounds", func(d *Declaration) { d.MaxSteps = 5 }, "max_rounds"},
	}
	for _, c := range cases {
		d := base()
		c.mut(d)
		if _, err := Build(context.Background(), d, Deps{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("[%s] want assembly error mentioning %q, got %v", c.name, c.want, err)
		}
	}
	// 未知 mode
	d := base()
	d.Mode = "inlien"
	if _, err := Build(context.Background(), d, Deps{}); err == nil || !strings.Contains(err.Error(), "inlien") {
		t.Fatalf("unknown mode must fail fast, got %v", err)
	}
}
