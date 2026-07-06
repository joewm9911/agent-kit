package capability

import (
	"context"
	"testing"
)

// TestNoParams:无参工具的 ToolInfo.ParamsOneOf 必须为 nil(空 schema 会被
// 部分厂商 400,eino FAQ 契约)。
func TestNoParams(t *testing.T) {
	c := New(Meta{Ref: Ref{Kind: "tool", Domain: "d", Name: "noargs"}, Params: NoParams},
		func(context.Context, string) (string, error) { return "ok", nil })
	tl, err := c.AsTool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ParamsOneOf != nil {
		t.Fatal("NoParams must yield nil ParamsOneOf")
	}
	// 常规默认:nil Params → 单 input 参数,不受影响
	c2 := New(Meta{Ref: Ref{Kind: "tool", Domain: "d", Name: "hasargs"}},
		func(context.Context, string) (string, error) { return "ok", nil })
	tl2, _ := c2.AsTool(context.Background())
	info2, _ := tl2.Info(context.Background())
	if info2.ParamsOneOf == nil {
		t.Fatal("default single-input param must remain")
	}
}
