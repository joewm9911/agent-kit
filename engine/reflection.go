package engine

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

func init() {
	Register("reflection", BuildReflection)
}

const defaultReviewerPrompt = `你是评审者。按任务要求严格检查草稿,输出 JSON:
- 通过 → {"pass": true}
- 不通过 → {"pass": false, "feedback": "具体、可执行的修改意见,逐条列出"}
只输出 JSON。宁可多挑一轮,不要放过明显问题。`

const defaultReflectExecutorPrompt = `你是执行者。完成给定任务;收到评审意见时,针对每条意见修正上一稿,输出完整的新稿(不是差量)。`

// BuildReflection 构建反思引擎(Evaluator-Optimizer):
//
//	执行者(react,可带工具)产稿 → 评审者(模型)按标准挑错 →
//	执行者带意见修正 → 循环,直到评审通过或轮次耗尽。
//
// 循环推进、终止条件、轮次上限由代码保证;草稿内容、评审判断由模型
// 决定——与 plan-execute 同一构造哲学。适合有明确质量标准的产出
// (报告、代码、结构化计划)。确定性校验(schema、存在性检查)应做成
// 执行者工具面上的工具,评审者只管代码检查不了的质量维度。
//
// engine_config:
//
//	max_rounds     评审轮次上限,默认 3
//	on_exhausted   轮次耗尽策略:best(默认,返回最新稿)| error
//	reviewer_prompt / executor_prompt  覆盖内置提示词
func BuildReflection(ctx context.Context, asm *Assembly) (Runner, error) {
	executor, err := BuildReAct(ctx, &Assembly{
		Model:        asm.Model,
		Capabilities: asm.Capabilities,
		MaxSteps:     asm.ConfInt("step_max_steps", 10),
		Modifier:     systemPrepender(promptOr(asm, "executor", defaultReflectExecutorPrompt)),
		Rewriter:     asm.Rewriter,
	})
	if err != nil {
		return nil, err
	}
	return &reflectionRunner{
		asm:       asm,
		executor:  executor,
		reviewer:  promptOr(asm, "reviewer", defaultReviewerPrompt),
		maxRounds: asm.ConfInt("max_rounds", 3),
		exhausted: asm.ConfString("on_exhausted", "best"),
	}, nil
}

type reflectionRunner struct {
	asm       *Assembly
	executor  Runner
	reviewer  string
	maxRounds int
	exhausted string
}

type review struct {
	Pass     bool   `json:"pass"`
	Feedback string `json:"feedback"`
}

func (r *reflectionRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	task := renderConversation(msgs)

	draft, err := r.executor.Generate(ctx, []*schema.Message{schema.UserMessage("任务:\n" + task)})
	if err != nil {
		return nil, fmt.Errorf("reflection draft: %w", err)
	}

	for round := 0; round < r.maxRounds; round++ {
		out, err := r.asm.Model.Generate(ctx, []*schema.Message{
			schema.SystemMessage(r.reviewer),
			schema.UserMessage(fmt.Sprintf("任务:\n%s\n\n当前稿:\n%s", task, draft.Content)),
		})
		if err != nil {
			return nil, fmt.Errorf("reflection review: %w", err)
		}
		var v review
		if err := unmarshalLoose(out.Content, &v); err != nil {
			return nil, fmt.Errorf("reflection review: parse %q: %w", out.Content, err)
		}
		if v.Pass {
			return draft, nil
		}
		draft, err = r.executor.Generate(ctx, []*schema.Message{schema.UserMessage(fmt.Sprintf(
			"任务:\n%s\n\n上一稿:\n%s\n\n评审意见(逐条修正):\n%s", task, draft.Content, v.Feedback))})
		if err != nil {
			return nil, fmt.Errorf("reflection revise: %w", err)
		}
	}

	if r.exhausted == "error" {
		return nil, fmt.Errorf("reflection: max rounds (%d) exhausted without passing review", r.maxRounds)
	}
	return draft, nil // best:轮次耗尽返回最新稿,不丢进度
}

func (r *reflectionRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return singleAsStream(r.Generate(ctx, msgs))
}
