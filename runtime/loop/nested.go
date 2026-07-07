// nested.go:组件内部模型调用的 callback 切面。digest 摘要、压缩摘要、
// 守卫弹回等"包装器内部再调模型"的场景,graph 节点切面只覆盖最外层
// Generate,内层调用对进度/tracing 不可见(实测一次 46s 的黑箱 span 里
// 藏着三次弹回)。按 eino 文档建议:内层调用用 ReuseHandlers 换 RunInfo
// 并自行上报 OnStart/OnEnd/OnError,使其成为独立可见的子 span。
package loop

import (
	"context"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
)

// observedGenerate 执行一次内层模型调用并上报 callback 切面(RunInfo.Name
// = name,Component = ChatModel)。无 handler 时零开销透传。
func observedGenerate(ctx context.Context, name string,
	gen func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error),
	msgs []*schema.Message) (*schema.Message, error) {

	// 内部动作标记:进度事件据此把这类模型调用归为 builtin
	// (框架辅助生成,非业务轮次)。
	ctx = runctx.WithBuiltinStep(ctx)
	ctx = callbacks.ReuseHandlers(ctx, &callbacks.RunInfo{
		Name: name, Component: components.ComponentOfChatModel,
	})
	ctx = callbacks.OnStart(ctx, &einomodel.CallbackInput{Messages: msgs})
	out, err := gen(ctx, msgs)
	if err != nil {
		callbacks.OnError(ctx, err)
		return out, err
	}
	callbacks.OnEnd(ctx, &einomodel.CallbackOutput{Message: out})
	return out, nil
}
