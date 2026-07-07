// progress.go:能力执行的进度事件发射门(Ring 0)。
//
// 事件从能力包装层发射而非 eino 回调:这里拿得到 capability.Meta 的
// 真值(Ref.Kind/Domain/Name)——skill 和 tool 在 eino 眼里都是 Tool,
// 只有能力层分得清。模型事件仍由 observe 切面发射(observe.ProgressEvents
// 已不再发 Tool 事件,两边不双发)。无订阅零开销(EmitProgress 判 nil)。
package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// progressDetailMax 是事件 Detail 的截断上限(防大 payload 涌进订阅队列)。
const progressDetailMax = 120

// ProgressTools 给能力集套上进度发射(应用在门链最外层:事件时长
// 对齐"模型发起调用到结果回来"的用户体感,含审批等待)。
func ProgressTools(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &progressCap{inner: c})
	}
	return out
}

type progressCap struct{ inner capability.Capability }

func (p *progressCap) Meta() capability.Meta { return p.inner.Meta() }

func (p *progressCap) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := p.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", p.inner.Meta().Ref)
	}
	return &progressTool{inner: inv, meta: p.inner.Meta()}, nil
}

func (p *progressCap) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return emitAround(ctx, p.inner.Meta(), argsJSON, func(ctx context.Context) (string, error) {
			return capability.Invoke(ctx, p.inner, argsJSON)
		})
	}), nil
}

type progressTool struct {
	inner tool.InvokableTool
	meta  capability.Meta
}

func (t *progressTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return t.inner.Info(ctx) }

func (t *progressTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return emitAround(ctx, t.meta, argsJSON, func(ctx context.Context) (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}

// emitAround 发射 start,执行,按结果发射 done/error。
func emitAround(ctx context.Context, meta capability.Meta, argsJSON string, exec func(ctx context.Context) (string, error)) (string, error) {
	base := runctx.ProgressEvent{
		Scope:     runctx.Scope(ctx),
		ScopeKind: capScopeKind(meta),
		CapKind:   meta.Ref.Kind,
		Domain:    meta.Ref.Domain,
		Name:      meta.Ref.Name,
	}
	start := base
	start.Status, start.Detail = "start", clipDetail(argsJSON)
	runctx.EmitProgress(ctx, start)

	t0 := time.Now()
	out, err := exec(ctx)
	end := base
	end.Dur = time.Since(t0)
	if err != nil {
		end.Status, end.Detail = "error", clipDetail(err.Error())
	} else {
		end.Status, end.Detail = "done", clipDetail(out)
	}
	runctx.EmitProgress(ctx, end)
	return out, err
}

// capScopeKind:builtin 域的能力是框架内部模块,其余是用户配置进来的。
func capScopeKind(meta capability.Meta) string {
	if meta.Ref.Domain == "builtin" {
		return runctx.ScopeBuiltin
	}
	return runctx.ScopeCustom
}

func clipDetail(s string) string {
	r := []rune(s)
	if len(r) <= progressDetailMax {
		return s
	}
	return string(r[:progressDetailMax]) + "..."
}
