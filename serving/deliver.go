// deliver.go:交付物出站解析——"引用即附带"。终答里引用的 #dN 由框架
// 展开随行(大脑只行使策展权);always 语义不待引用恒随行。
// 设计:docs/deliverable-channel-plan.md §2.3。
package serving

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

var deliverRefRe = regexp.MustCompile(`#(d\d+)`)

// 护栏:防模型全量引用绕过策展。超限按序保留、Warn 留证。
const (
	maxDeliverables    = 5
	maxDeliverablesLen = 200 << 10 // 200KB(内容总量)
)

// collapseBareReference 处理"终答只有引用"的形态:剥掉全部 #dN 引用与
// 空白/标点后所剩无几时,终答本身没有信息量——用首个交付物原文顶替
// 终答,该交付物不再随行(其余照旧)。返回处理后的 (answer, dels)。
func collapseBareReference(answer string, dels []runctx.Deliverable) (string, []runctx.Deliverable) {
	if len(dels) == 0 {
		return answer, dels
	}
	stripped := deliverRefRe.ReplaceAllString(answer, "")
	stripped = strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \t\n\r,。;;:.、!!??()()[]【】·-—*#|", r) {
			return -1
		}
		return r
	}, stripped)
	if len([]rune(stripped)) > 12 { // 有实质导读:保持导读+随行
		return answer, dels
	}
	return dels[0].Content, dels[1:]
}

// deliverMergeMax 是"单交付物合并进终答卡"的尺寸门(导读+原文字节数)。
// 取值保守对齐最紧的通道约束(飞书卡片约 28KB,留出卡片结构与转义开销):
// 超过它的交付物合并后会被适配器的护栏截断——砍的是交付物本体,恰好
// 毁掉零损耗承诺,所以宁可两条。精确的通道级定制归装饰器。
const deliverMergeMax = 16 << 10

// mergeSingleDeliverable 处理最高频形态:恰好一份交付物且尺寸在门内时,
// 原文合并进终答卡(导读在上、原文紧随),单条送达;多份或超门维持
// "导读卡 + 随行卡"。返回处理后的 (answer, 剩余随行清单)。
func mergeSingleDeliverable(answer string, dels []runctx.Deliverable) (string, []runctx.Deliverable) {
	if len(dels) != 1 || len(answer)+len(dels[0].Content) > deliverMergeMax {
		return answer, dels
	}
	return answer + "\n\n---\n\n" + dels[0].Content, nil
}

// ResolveDeliverables 是裸跑宿主(CLI 等自己调 agent.Run 的场景)用的
// 出站解析入口,语义同 dispatcher/HTTP 内部路径。
func ResolveDeliverables(answer string, sink *runctx.DeliverableSink) []runctx.Deliverable {
	return resolveDeliverables(answer, sink, nil)
}

// resolveDeliverables 按终答引用解析随行清单:引用出现序优先,always
// 语义追加在后;同 id 只随行一次;幻觉引用忽略并记日志。
func resolveDeliverables(answer string, sink *runctx.DeliverableSink, logger *slog.Logger) []runctx.Deliverable {
	items := sink.Items()
	if len(items) == 0 {
		return nil
	}
	byID := make(map[string]runctx.Deliverable, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	var out []runctx.Deliverable
	seen := map[string]bool{}
	push := func(d runctx.Deliverable) {
		if seen[d.ID] {
			return
		}
		seen[d.ID] = true
		out = append(out, d)
	}
	for _, m := range deliverRefRe.FindAllStringSubmatch(answer, -1) {
		if d, ok := byID[m[1]]; ok {
			push(d)
		} else if logger != nil {
			// 模型幻觉引用不炸轮,但要留证:引用行为本身是批 4 A/B 的观测面。
			logger.Warn("answer references unknown deliverable id", slog.String("id", m[1]))
		}
	}
	for _, it := range items {
		if it.Mode == capability.DeliverAlways {
			push(it)
		}
	}
	// 护栏裁剪
	total := 0
	for i, d := range out {
		total += len(d.Content)
		if i >= maxDeliverables || total > maxDeliverablesLen {
			if logger != nil {
				logger.Warn("deliverables truncated by guard", slog.Int("kept", i), slog.Int("dropped", len(out)-i))
			}
			out = out[:i]
			break
		}
	}
	return out
}
