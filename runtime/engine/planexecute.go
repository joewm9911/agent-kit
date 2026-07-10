package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

func init() {
	Register("plan-execute", BuildPlanExecute)
}

const defaultPlannerPrompt = `You are a task planner. Break the user's goal into as few independently executable steps as possible.
Output JSON only, in the form: {"steps": ["step 1", "step 2", ...]}`

const defaultReplannerPrompt = `You are a task reviewer. Based on the goal and the results of completed steps, decide:
- goal achieved → output {"action": "finish", "response": "the final answer for the user"}
- not yet achieved → output {"action": "continue", "steps": ["remaining or revised steps", ...]}
Output JSON only.`

const defaultExecutorPrompt = `You are an executor. Complete only the single step given to you, and output that step's result.`

// BuildPlanExecute 构建 Plan-Execute 引擎:
//
//	planner(拆解计划)→ executor(ReAct 逐步执行)→ replanner(判断完成/修正)循环。
//
// 循环推进、终止条件、轮次上限由代码保证;计划内容、每步执行、完成
// 判断由模型决定。该引擎不面向 agent 配置,由 skill 声明经
// engine: plan-execute 引用,打包成能力后挂到上级大脑的工具面。
func BuildPlanExecute(ctx context.Context, asm *Assembly) (Runner, error) {
	if _, legacy := asm.Config["step_max_steps"]; legacy {
		return nil, fmt.Errorf("engine_config.step_max_steps has been renamed step_max_rounds (its meaning was always rounds)")
	}
	executor, err := BuildReAct(ctx, &Assembly{
		Model:        asm.Model,
		Capabilities: asm.Capabilities,
		MaxSteps:     asm.ConfInt("step_max_rounds", 10),
		Modifier:     stageLoopModifier(asm, promptOr(asm, "executor", defaultExecutorPrompt)),
		Rewriter:     asm.Rewriter,
	})
	if err != nil {
		return nil, err
	}
	return &planExecuteRunner{
		asm:       asm,
		executor:  executor,
		planner:   promptOr(asm, "planner", defaultPlannerPrompt),
		replanner: promptOr(asm, "replanner", defaultReplannerPrompt),
		maxRounds: asm.ConfInt("max_rounds", 3),
		exhausted: asm.ConfString("on_exhausted", "summarize"),
	}, nil
}

func promptOr(asm *Assembly, key, def string) string {
	if p, ok := asm.Prompts[key]; ok && p != "" {
		return p
	}
	return def
}

// stageLoopModifier 组装多阶段引擎内部 executor(角色①:有工具面的循环调用)
// 的系统消息:先经上游 Modifier(L1 纪律 + persona 身份 + 环境,由组件装配注入),
// 再把 executor 阶段提示词续进同一条系统消息——职能层不顶掉纪律层与身份层。
// 无上游 Modifier(直连引擎/测试)退化为纯前置(与旧 systemPrepender 同行为)。
func stageLoopModifier(asm *Assembly, stagePrompt string) MessageModifier {
	return func(ctx context.Context, msgs []*schema.Message) []*schema.Message {
		sp := renderStage(ctx, stagePrompt)
		if asm.Modifier == nil {
			return append([]*schema.Message{schema.SystemMessage(sp)}, msgs...)
		}
		out := asm.Modifier(ctx, msgs)
		if len(out) > 0 && out[0].Role == schema.System {
			head := *out[0]
			head.Content += "\n\n# Stage role\n" + sp
			return append([]*schema.Message{&head}, out[1:]...)
		}
		return append([]*schema.Message{schema.SystemMessage(sp)}, out...)
	}
}

type planExecuteRunner struct {
	asm       *Assembly
	executor  Runner
	planner   string
	replanner string
	maxRounds int    // replan 轮次上限,防止循环不收敛
	exhausted string // 轮次耗尽策略:summarize | error
}

type plan struct {
	Steps []string `json:"steps"`
}

type verdict struct {
	Action   string   `json:"action"`
	Response string   `json:"response"`
	Steps    []string `json:"steps"`
}

func (r *planExecuteRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	goal := renderConversation(msgs)

	// 1. Plan
	var p plan
	if err := r.generateJSON(ctx, r.planner, "目标:\n"+goal, &p); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if len(p.Steps) == 0 {
		p.Steps = []string{goal}
	}

	var done []string // 已执行步骤及结果,作为后续步骤和复盘的上下文
	steps := p.Steps
	for round := 0; round < r.maxRounds; round++ {
		// 2. Execute:逐步执行,携带既往结果
		for _, step := range steps {
			exeInput := fmt.Sprintf("Goal: %s\n\nCompleted steps:\n%s\nCurrent step: %s",
				goal, strings.Join(done, "\n"), step)
			out, err := r.executor.Generate(ctx, []*schema.Message{schema.UserMessage(exeInput)})
			if err != nil {
				return nil, fmt.Errorf("execute step %q: %w", step, err)
			}
			done = append(done, fmt.Sprintf("- %s => %s", step, out.Content))
		}

		// 3. Replan:判断目标是否达成
		var v verdict
		reviewInput := fmt.Sprintf("Goal: %s\n\nCompleted steps and results:\n%s", goal, strings.Join(done, "\n"))
		if err := r.generateJSON(ctx, r.replanner, reviewInput, &v); err != nil {
			return nil, fmt.Errorf("replan: %w", err)
		}
		if v.Action == "finish" || len(v.Steps) == 0 {
			return schema.AssistantMessage(v.Response, nil), nil
		}
		steps = v.Steps
	}

	if r.exhausted == "error" {
		return nil, fmt.Errorf("plan-execute: max rounds (%d) exhausted", r.maxRounds)
	}
	// 轮次耗尽:让模型基于已有结果收尾,不丢弃进度。
	out, err := r.asm.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(stageSystem(ctx, "Based on the execution record below, give the best final answer to the goal.")),
		schema.UserMessage(fmt.Sprintf("Goal: %s\n\nExecution record:\n%s", goal, strings.Join(done, "\n"))),
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stream:plan-execute 的中间过程不适合流式,仅最终结果转为流返回。
// 过程可见性走 observe 轨迹。
func (r *planExecuteRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

func (r *planExecuteRunner) generateJSON(ctx context.Context, system, user string, target any) error {
	out, err := r.asm.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(stageSystem(ctx, system)), // planner/replanner:persona + 阶段提示词(角色②)
		schema.UserMessage(user),
	})
	if err != nil {
		return err
	}
	if err := unmarshalLoose(out.Content, target); err != nil {
		return fmt.Errorf("parse model output %q: %w", out.Content, err)
	}
	return nil
}

func renderConversation(msgs []*schema.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	return sb.String()
}
