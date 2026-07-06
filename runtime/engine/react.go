package engine

import (
	"fmt"

	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

func init() {
	Register("react", BuildReAct)
}

// BuildReAct 基于 eino 内置的 ReAct agent 构建 Runner:
// 模型在「思考 → 调工具 → 观察结果」的循环里自主推进,直到不再产生
// 工具调用。这是"大脑即循环"的主形态,是否完成由模型停止调用工具
// 自然表达,MaxSteps 是流程兜底。
func BuildReAct(ctx context.Context, asm *Assembly) (Runner, error) {
	tools, err := capability.AsTools(ctx, asm.Capabilities)
	if err != nil {
		return nil, err
	}

	// max_steps 的对外语义是"工具调用轮数"(直觉语义);eino 的 MaxStep 按
	// 节点转移计数,一轮 = ChatModel + Tools = 2 步,最后必须以 ChatModel
	// 收尾——换算 2N+1,让配置 N 恰好允许 N 轮工具调用 + 一次收尾作答。
	rounds := asm.MaxSteps
	if rounds <= 0 {
		rounds = 12
	}

	cfg := &react.AgentConfig{
		ToolCallingModel: asm.Model,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
			// 工具名幻觉防御:默认行为是 ToolsNode 报错→整轮失败;转成结果
			// 字符串让模型自纠(与"错误作结果回传"的既有哲学一致)。
			UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
				return "工具 " + name + " 不存在。可用工具见工具列表,请改用真实存在的工具或直接作答。", nil
			},
			// 工具错误转结果(同一哲学的兜底面):ToolsNode 对工具 error 透明
			// 传播→整轮失败,模型幻觉一次坏 JSON 参数即可炸轮(实测 exectool/
			// httptool 等解析失败返回 error)。统一转成结果字符串让模型自纠;
			// HITL 中断类错误必须放行,否则审批/挂起中断会被吞。
			ToolCallMiddlewares: []compose.ToolMiddleware{{
				Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
					return func(ctx context.Context, in *compose.ToolInput) (*compose.ToolOutput, error) {
						out, err := next(ctx, in)
						if err != nil {
							if turnTerminal(err) {
								return out, err // 挂起/用户中断/预算/HITL 中断:必须穿透
							}
							return &compose.ToolOutput{Result: fmt.Sprintf(
								"工具 %s 执行失败:%v。请读取错误原因,修正参数后重试一次或换用其他方式。", in.Name, err)}, nil
						}
						return out, err
					}
				},
			}},
		},
		MaxStep: rounds*2 + 1,
		// 流式工具调用判定:默认实现按首个非空包判断,"先文本后工具调用"
		// 的模型(含 reasoning 前置)会被误判为终答。改为读到工具调用或
		// EOF 才下结论——正确性优先,代价是纯文本终答的分支判定要等流
		// 收尾(react 内部消费的是流副本,不影响对外流式输出)。
		StreamToolCallChecker: streamToolCallChecker,
	}
	if asm.Modifier != nil {
		cfg.MessageModifier = react.MessageModifier(asm.Modifier)
	}
	if asm.Rewriter != nil {
		cfg.MessageRewriter = react.MessageModifier(asm.Rewriter)
	}

	// 无工具时退化为单次模型调用,不进循环。
	if len(tools) == 0 {
		return &bareModelRunner{asm: asm}, nil
	}

	ag, err := react.NewAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &reactRunner{ag: ag}, nil
}

// turnTerminal 判定错误是否"轮次终止级"——必须穿透工具错误兜底:
// 挂起(suspend.ErrSuspended)、用户中断(loop.ErrInterrupted)、预算硬停
// (loop.ErrBudgetExhausted)经 TurnTerminal 标记接口自 declare(engine
// 不能依赖 loop,以接口解耦);compose 的 HITL 中断经官方判定放行。
func turnTerminal(err error) bool {
	var tt interface{ TurnTerminal() }
	if errors.As(err, &tt) {
		return true
	}
	_, isInterrupt := compose.IsInterruptRerunError(err)
	return isInterrupt
}

// streamToolCallChecker 读流直到看到工具调用或 EOF:兼容"先文本/推理、
// 后工具调用"的模型(eino 默认按首个非空包判定,这类模型会被误判终答)。
func streamToolCallChecker(_ context.Context, sr *schema.StreamReader[*schema.Message]) (bool, error) {
	defer sr.Close()
	for {
		msg, err := sr.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		if len(msg.ToolCalls) > 0 {
			return true, nil
		}
	}
}

type reactRunner struct {
	ag *react.Agent
}

func (r *reactRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	return r.ag.Generate(ctx, msgs)
}

func (r *reactRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return r.ag.Stream(ctx, msgs)
}

type bareModelRunner struct {
	asm *Assembly
}

func (r *bareModelRunner) prepare(ctx context.Context, msgs []*schema.Message) []*schema.Message {
	if r.asm.Rewriter != nil {
		msgs = r.asm.Rewriter(ctx, msgs)
	}
	if r.asm.Modifier != nil {
		msgs = r.asm.Modifier(ctx, msgs)
	}
	return msgs
}

func (r *bareModelRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	return r.asm.Model.Generate(ctx, r.prepare(ctx, msgs))
}

func (r *bareModelRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return r.asm.Model.Stream(ctx, r.prepare(ctx, msgs))
}
