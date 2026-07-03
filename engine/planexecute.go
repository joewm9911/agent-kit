package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

func init() {
	Register("plan-execute", BuildPlanExecute)
}

const defaultPlannerPrompt = `你是任务规划器。把用户目标拆解为尽量少的、可独立执行的步骤。
只输出 JSON,格式:{"steps": ["步骤1", "步骤2", ...]}`

const defaultReplannerPrompt = `你是任务复盘器。根据目标与已完成步骤的结果判断:
- 目标已达成 → 输出 {"action": "finish", "response": "给用户的最终回答"}
- 尚未达成 → 输出 {"action": "continue", "steps": ["剩余或修正后的步骤", ...]}
只输出 JSON。`

const defaultExecutorPrompt = `你是执行器。只完成当前给定的这一个步骤,输出该步骤的执行结果。`

// BuildPlanExecute 构建 Plan-Execute 引擎:
//
//	planner(拆解计划)→ executor(ReAct 逐步执行)→ replanner(判断完成/修正)循环。
//
// 循环推进、终止条件、轮次上限由代码保证;计划内容、每步执行、完成
// 判断由模型决定。该引擎不面向 agent 配置,由 skill 声明经
// engine: plan-execute 引用,打包成能力后挂到上级大脑的工具面。
func BuildPlanExecute(ctx context.Context, asm *Assembly) (Runner, error) {
	executor, err := BuildReAct(ctx, &Assembly{
		Model:        asm.Model,
		Capabilities: asm.Capabilities,
		MaxSteps:     asm.ConfInt("step_max_steps", 10),
		Modifier:     systemPrepender(promptOr(asm, "executor", defaultExecutorPrompt)),
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

func systemPrepender(sys string) MessageModifier {
	return func(_ context.Context, msgs []*schema.Message) []*schema.Message {
		return append([]*schema.Message{schema.SystemMessage(sys)}, msgs...)
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
			exeInput := fmt.Sprintf("目标:%s\n\n已完成步骤:\n%s\n当前步骤:%s",
				goal, strings.Join(done, "\n"), step)
			out, err := r.executor.Generate(ctx, []*schema.Message{schema.UserMessage(exeInput)})
			if err != nil {
				return nil, fmt.Errorf("execute step %q: %w", step, err)
			}
			done = append(done, fmt.Sprintf("- %s => %s", step, out.Content))
		}

		// 3. Replan:判断目标是否达成
		var v verdict
		reviewInput := fmt.Sprintf("目标:%s\n\n已完成步骤及结果:\n%s", goal, strings.Join(done, "\n"))
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
		schema.SystemMessage("根据以下执行记录,尽力给出目标的最终回答。"),
		schema.UserMessage(fmt.Sprintf("目标:%s\n\n执行记录:\n%s", goal, strings.Join(done, "\n"))),
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
		schema.SystemMessage(system),
		schema.UserMessage(user),
	})
	if err != nil {
		return err
	}
	raw := ExtractJSON(out.Content)
	if err := json.Unmarshal([]byte(raw), target); err != nil {
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
