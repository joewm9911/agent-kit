package suspend

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

// DurableEffects 给能力集套上效果日志闸门(Ring 0):mutating 能力的
// 执行结果随轮记录,重放(挂起恢复后的重跑)命中即返回记录结果、
// 不二次执行——审批过的转账不会因为恢复而转两次。只读能力照常重跑。
// ctx 无日志(非挂起模式)时零开销。
//
// 套闸顺序:审批之内、截断之外——重放时先由交互日志越过审批,再由
// 本闸跳过执行;记录的是模型实际看到的(截断后)结果。
func DurableEffects(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		if c.Meta().Risk >= capability.RiskMutating {
			out = append(out, &durable{inner: c})
		} else {
			out = append(out, c)
		}
	}
	return out
}

type durable struct {
	inner capability.Capability
}

func (d *durable) Meta() capability.Meta { return d.inner.Meta() }

func (d *durable) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := d.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", d.inner.Meta().Ref)
	}
	return &durableTool{inner: inv, key: d.inner.Meta().Ref.Key()}, nil
}

func (d *durable) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	key := d.inner.Meta().Ref.Key()
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return runDurable(ctx, key, argsJSON, func(ctx context.Context) (string, error) {
			return capability.Invoke(ctx, d.inner, argsJSON)
		})
	}), nil
}

type durableTool struct {
	inner tool.InvokableTool
	key   string
}

func (t *durableTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *durableTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return runDurable(ctx, t.key, argsJSON, func(ctx context.Context) (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}

func runDurable(ctx context.Context, capKey, argsJSON string, exec func(ctx context.Context) (string, error)) (string, error) {
	j := JournalFrom(ctx)
	if j == nil {
		return exec(ctx)
	}
	if out, ok := j.Effect(ctx, capKey, argsJSON); ok {
		return out, nil // 重放:已执行过(或命中在途哨兵),直接返回记录结果
	}
	// 两阶段台账:执行前先落"已开始"标记。此写失败即拒绝执行——副作用尚未
	// 发生,失败安全;若跳过标记直接执行,结果写失败会让重放二次执行已审批
	// 的 mutating 操作(效果日志存在的全部意义就是防这一下)。
	if err := j.BeginEffect(ctx, capKey, argsJSON); err != nil {
		return "", fmt.Errorf("effect journal unavailable, refusing to execute mutating capability (retry later): %w", err)
	}
	out, err := exec(ctx)
	if err != nil {
		return out, err
	}
	j.SaveEffect(ctx, capKey, argsJSON, out)
	return out, nil
}
