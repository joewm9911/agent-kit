package main

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/protocol/channel"
)

func TestTablesToList(t *testing.T) {
	in := "在售商品:\n| SKU | 名称 | 品类 |\n|---|---|---|\n| P100 | 降噪耳机 | 音频 |\n| P103 | 电竞耳机 | 音频 |\n合计 2 款。"
	got := tablesToList(in)
	if strings.Contains(got, "|---") {
		t.Fatalf("separator leaked: %q", got)
	}
	for _, want := range []string{"在售商品:", "**P100** 名称: 降噪耳机 · 品类: 音频", "**P103**", "合计 2 款。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	// 非表格内容原样保留
	if plain := tablesToList("没有表格的普通文本"); plain != "没有表格的普通文本" {
		t.Fatalf("plain text mutated: %q", plain)
	}
}

func TestOpsCardShape(t *testing.T) {
	out := opsCard(context.Background(), channel.ConvRef{}, channel.Outbound{
		Kind: channel.KindAnswer, Text: "答案",
		Progress: []string{"✓ 查库存 (1.2s)"}, Meta: "耗时 3.0s · 1 次工具调用",
	})
	if out.Native == nil {
		t.Fatal("answer must produce native card")
	}
	header := out.Native["header"].(map[string]any)
	if header["template"] != "blue" {
		t.Fatalf("answer header = %+v", header)
	}
	els := out.Native["elements"].([]any)
	if len(els) != 4 { // 折叠面板 + hr + 正文 + note
		t.Fatalf("elements = %d, want 4", len(els))
	}
	if els[0].(map[string]any)["tag"] != "collapsible_panel" {
		t.Fatalf("first element should be progress panel: %+v", els[0])
	}
	// 处理中:面板展开、灰头
	p := opsCard(context.Background(), channel.ConvRef{}, channel.Outbound{
		Kind: channel.KindProcessing, Text: "⏳ 处理中...", Progress: []string{"⚙ 查库存 执行中"},
	})
	if p.Native["header"].(map[string]any)["template"] != "grey" {
		t.Fatal("processing header should be grey")
	}
	if p.Native["elements"].([]any)[0].(map[string]any)["expanded"] != true {
		t.Fatal("processing panel should be expanded")
	}
	// 杂项通知不接管
	if n := opsCard(context.Background(), channel.ConvRef{}, channel.Outbound{Text: "通知"}); n.Native != nil {
		t.Fatal("zero-kind must keep default rendering")
	}
}
