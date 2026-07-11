package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

func init() {
	Register("direct", BuildDirect)
}

// BuildDirect 构建单发引擎:一次模型调用(绑定工具),若产生工具调用
// 则并行执行这一轮,再以结果做一次**无工具**的收尾调用。最多两次
// 模型调用,没有循环——"提示词 + 工具,按提示词执行给结果"的最简
// 执行形态。适合动作确定、只需模型组织入参与总结结果的场景;需要
// 模型看结果后再决定下一步的,用 react。
func BuildDirect(ctx context.Context, asm *Assembly) (Runner, error) {
	tools, err := capability.AsTools(ctx, asm.Capabilities)
	if err != nil {
		return nil, err
	}
	infos := make([]*schema.ToolInfo, 0, len(tools))
	invokables := make(map[string]tool.InvokableTool, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, err
		}
		inv, ok := t.(tool.InvokableTool)
		if !ok {
			return nil, fmt.Errorf("direct: tool %q is not invokable", info.Name)
		}
		infos = append(infos, info)
		invokables[info.Name] = inv
	}
	return &directRunner{asm: asm, infos: infos, tools: invokables}, nil
}

type directRunner struct {
	asm   *Assembly
	infos []*schema.ToolInfo
	tools map[string]tool.InvokableTool
}

func (r *directRunner) prepare(ctx context.Context, msgs []*schema.Message) []*schema.Message {
	if r.asm.Rewriter != nil {
		msgs = r.asm.Rewriter(ctx, msgs)
	}
	if r.asm.Modifier != nil {
		msgs = r.asm.Modifier(ctx, msgs)
	}
	return msgs
}

func (r *directRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	msgs = r.prepare(ctx, msgs)

	bound := r.asm.Model
	if len(r.infos) > 0 {
		var err error
		if bound, err = r.asm.Model.WithTools(r.infos); err != nil {
			return nil, err
		}
	}
	first, err := bound.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	if len(first.ToolCalls) == 0 {
		return first, nil // 模型直接作答,单次调用结束
	}

	// 并行执行本轮全部工具调用,结果按声明顺序回填
	results := make([]*schema.Message, len(first.ToolCalls))
	terminals := make([]error, len(first.ToolCalls))
	var wg sync.WaitGroup
	for i, call := range first.ToolCalls {
		wg.Add(1)
		go func(i int, call schema.ToolCall) {
			defer wg.Done()
			inv, ok := r.tools[call.Function.Name]
			if !ok {
				results[i] = schema.ToolMessage(fmt.Sprintf("调用失败:工具 %q 不存在。只能调用本轮提供的工具。", call.Function.Name), call.ID)
				return
			}
			out, err := inv.InvokableRun(ctx, call.Function.Arguments)
			if err != nil {
				// 轮次终止级(挂起/中断/预算硬停)必须穿透,不能压成结果
				// 字符串——否则 direct 组件内的 ask_user 挂起被吞,终答照跑。
				if turnTerminal(err) {
					terminals[i] = err
					return
				}
				// 其余以结果回传错误,让收尾调用能向上说明,不中断
				results[i] = schema.ToolMessage(fmt.Sprintf("工具执行失败:%v。可修正参数重试或换路径,不要原样重试。", err), call.ID)
				return
			}
			results[i] = schema.ToolMessage(out, call.ID)
		}(i, call)
	}
	wg.Wait()
	for _, terr := range terminals {
		if terr != nil {
			return nil, terr
		}
	}

	// 收尾:无工具的最终调用,强制给出结果(不给继续调用的机会)
	final := append(append(msgs, first), results...)
	return r.asm.Model.Generate(ctx, final)
}

// Stream:单发引擎的中间过程不流式,仅最终结果转为流返回。
func (r *directRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}
