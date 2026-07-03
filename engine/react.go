package engine

import (
	"context"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/cloverzhang/agent-kit/capability"
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

	maxStep := asm.MaxSteps
	if maxStep <= 0 {
		maxStep = 25
	}

	cfg := &react.AgentConfig{
		ToolCallingModel: asm.Model,
		ToolsConfig:      compose.ToolsNodeConfig{Tools: tools},
		MaxStep:          maxStep,
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
