// card.go:ops-card 装饰器——第三方定制卡片的参考实现
// (机制见 docs/channel-card-design.md;框架只给事实,对原始输出内容
// 的一切定制都在这里,包括表格降级这类飞书特性适配)。
//
// 形态:
//
//	┌ 标题头(按 Kind 换色:处理中灰 / 完成蓝 / 提问橙 / 失败红)
//	├ 折叠面板「执行过程」(out.Progress;处理中展开,完成后收起)
//	├ 正文(markdown 表格转分组列表——飞书卡片不渲染表格语法)
//	└ note 灰字(out.Meta:耗时/调用数)
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/serving"
)

func init() {
	serving.RegisterDecorator("ops-card", opsCard)
}

var cardStyle = map[string]struct{ title, color string }{
	channel.KindProcessing: {"运营助手 · 处理中", "grey"},
	channel.KindAnswer:     {"运营助手", "blue"},
	channel.KindQuestion:   {"需要你确认", "orange"},
	channel.KindError:      {"处理失败", "red"},
}

// progressVisible 是过程区直接可见的行数上限;更早的步骤收进
// 「历史步骤」折叠面板(飞书卡片无滚动条组件,折叠是唯一收纳形态)。
const progressVisible = 2

func opsCard(_ context.Context, _ channel.ConvRef, out channel.Outbound) channel.Outbound {
	style, ok := cardStyle[out.Kind]
	if !ok {
		return out // 杂项通知(中断确认等)保持默认渲染
	}
	var elements []any
	if len(out.Progress) > 0 {
		if out.Kind == channel.KindProcessing {
			// 处理中:最新 ≤2 行直接可见,更早的收进折叠面板
			recent := out.Progress
			if len(recent) > progressVisible {
				older := recent[:len(recent)-progressVisible]
				recent = recent[len(recent)-progressVisible:]
				elements = append(elements, panel(
					fmt.Sprintf("历史步骤(%d)", len(older)), older, false))
			}
			elements = append(elements, map[string]any{
				"tag": "markdown", "content": strings.Join(recent, "\n"),
			}, map[string]any{"tag": "hr"})
		} else {
			// 收口态:全过程收进可展开的折叠面板,历史永远可查
			elements = append(elements, panel(
				fmt.Sprintf("执行过程(%d 步)", len(out.Progress)), out.Progress, false),
				map[string]any{"tag": "hr"})
		}
	}
	elements = append(elements, map[string]any{
		"tag": "markdown", "content": tablesToList(out.Text),
	})
	if out.Meta != "" {
		elements = append(elements, map[string]any{
			"tag": "note", "elements": []any{map[string]any{"tag": "plain_text", "content": out.Meta}},
		})
	}
	out.Native = map[string]any{
		"config": map[string]any{"wide_screen_mode": true, "update_multi": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": style.title},
			"template": style.color,
		},
		"elements": elements,
	}
	return out
}

// panel 构造可展开的折叠面板(带展开箭头——没有 icon 的面板在客户端
// 上没有可点开的视觉标识,收起后像内容消失)。
func panel(title string, lines []string, expanded bool) map[string]any {
	return map[string]any{
		"tag":      "collapsible_panel",
		"expanded": expanded,
		"header": map[string]any{
			"title":               map[string]any{"tag": "markdown", "content": "**" + title + "**"},
			"vertical_align":      "center",
			"icon":                map[string]any{"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
			"icon_position":       "follow_text",
			"icon_expanded_angle": -180,
		},
		"elements": []any{map[string]any{"tag": "markdown", "content": strings.Join(lines, "\n")}},
	}
}

// tablesToList 把 markdown 表格转成飞书卡片能渲染的分组列表(飞书
// 卡片 markdown 组件不支持表格语法,真机实测管道符原样显示):
//
//	| SKU | 名称 | 品类 |
//	|---|---|---|            →   **P100** 名称: 降噪耳机 · 品类: 音频
//	| P100 | 降噪耳机 | 音频 |
//
// 非表格内容原样保留;识别不出表头分隔行的块不动(宁可不转不要转错)。
func tablesToList(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if !isTableRow(lines[i]) || i+1 >= len(lines) || !isTableSeparator(lines[i+1]) {
			out = append(out, lines[i])
			continue
		}
		header := splitRow(lines[i])
		i += 2 // 跳过表头与分隔行
		for ; i < len(lines) && isTableRow(lines[i]); i++ {
			out = append(out, rowToLine(header, splitRow(lines[i])))
		}
		i-- // 外层 i++ 补偿
	}
	return strings.Join(out, "\n")
}

func isTableRow(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
}

func isTableSeparator(s string) bool {
	t := strings.TrimSpace(s)
	if !isTableRow(t) {
		return false
	}
	for _, c := range t {
		switch c {
		case '|', '-', ':', ' ':
		default:
			return false
		}
	}
	return strings.Contains(t, "-")
}

func splitRow(s string) []string {
	t := strings.Trim(strings.TrimSpace(s), "|")
	cells := strings.Split(t, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// rowToLine:首列加粗作行标识,其余列按「表头: 值」拼接。
func rowToLine(header, row []string) string {
	if len(row) == 0 {
		return ""
	}
	var parts []string
	for i := 1; i < len(row) && i < len(header); i++ {
		if row[i] != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", header[i], row[i]))
		}
	}
	if len(parts) == 0 {
		return "**" + row[0] + "**"
	}
	return fmt.Sprintf("**%s** %s", row[0], strings.Join(parts, " · "))
}
