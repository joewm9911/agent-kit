// deliver.go:交付物出站解析——"引用即附带"。终答里引用的 #dN 由框架
// 展开随行(大脑只行使策展权);always 语义不待引用恒随行。
// 设计:docs/deliverable-channel-plan.md §2.3。
package serving

import (
	"log/slog"
	"regexp"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

var deliverRefRe = regexp.MustCompile(`#(d\d+)`)

// 护栏:防模型全量引用绕过策展。超限按序保留、Warn 留证。
const (
	maxDeliverables    = 5
	maxDeliverablesLen = 200 << 10 // 200KB(内容总量)
)

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
