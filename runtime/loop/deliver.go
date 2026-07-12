// deliver.go:交付物捕获(Ring 0)。标记了 Deliver 语义的能力返回后,
// 原文存底(复用结果暂存后端)并收进轮级 sink,给模型的结果前缀注入
// 引用标记——终答引用 #dN 即原文随行,大脑不再拥有转述权。
// 设计:docs/deliverable-channel-plan.md;链位:Dedup 内侧(回放不重捕)、
// Digest 内侧(捕获的是未消化原文)。
package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// DeliverResults 给能力集套上交付物捕获。所有能力都过调用计数(direct
// 判定需要全局调用序);仅 Meta.Deliver 非空的能力做捕获。
func DeliverResults(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &delivered{inner: c})
	}
	return out
}

type delivered struct {
	inner capability.Capability
}

func (d *delivered) Meta() capability.Meta { return d.inner.Meta() }

func (d *delivered) invoke(ctx context.Context, exec func() (string, error)) (string, error) {
	sink := runctx.DeliverableSinkFrom(ctx)
	seq := sink.NextCallSeq() // 所有调用计数,含证据类(sink 缺席 = 0,no-op)
	res, err := exec()
	meta := d.inner.Meta()
	if err != nil || sink == nil || meta.Deliver == capability.DeliverNone {
		return res, err
	}
	// 存底:复用结果暂存后端(与 digest 同族——一个为省上下文存原文,
	// 一个为保真交付存原文)。后端缺席/失败降级为轮内 id:本轮随行不受
	// 影响,只失去跨轮 read_result 取回;交付通路不因存储故障拉闸。
	var id string
	if rs := resultStoreFrom(ctx); rs != nil {
		id = rs.PutDeliver(ctx, res)
	}
	entry := sink.Emit(runctx.Deliverable{
		ID: id, Title: deliverTitle(meta.Ref.Name, res), Source: meta.Ref.String(),
		Mode: meta.Deliver, Content: res, Seq: seq,
	})
	// 标记由框架统一注入(非 skill 自述):格式可测、可整体下线。
	return fmt.Sprintf("[交付物#%s|%s] 已存底;终答中引用 #%s 即原文随行给用户,无需复述全文。\n%s",
		entry.ID, meta.Ref.Name, entry.ID, res), nil
}

// deliverTitle 取内容首个 markdown 标题作展示名,没有则用能力名。
func deliverTitle(capName, content string) string {
	for _, line := range strings.SplitN(content, "\n", 8) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			if t := strings.TrimSpace(strings.TrimLeft(line, "# ")); t != "" {
				return t
			}
		}
	}
	return capName
}

func (d *delivered) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := d.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", d.inner.Meta().Ref)
	}
	return &deliveredTool{d: d, inner: inv}, nil
}

func (d *delivered) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return d.invoke(ctx, func() (string, error) {
			return capability.Invoke(ctx, d.inner, argsJSON)
		})
	}), nil
}

type deliveredTool struct {
	d     *delivered
	inner tool.InvokableTool
}

func (t *deliveredTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return t.inner.Info(ctx) }

func (t *deliveredTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return t.d.invoke(ctx, func() (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}
