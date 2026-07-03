package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

func init() {
	Register("rewoo", BuildRewoo)
}

const defaultRewooPlannerPrompt = `你是规划器。把任务拆成一次性的工具调用计划,不依赖中间观察。
只输出 JSON:{"steps": [{"id": "e1", "tool": "<工具名>", "args": {<参数,值里可用 {e1} 引用前序步骤结果>}}, ...]}
规则:
- id 依次为 e1、e2、...;args 的字符串值里用 {eN} 占位引用第 N 步的结果;
- 只使用给定的工具;参数按各工具的说明组织;
- 步骤数不超过上限;能并行的步骤不要人为串联(没有引用关系就是可并行)。`

const defaultRewooSolverPrompt = `你是求解器。根据任务与各步骤的执行证据,直接给出最终回答。
证据可能包含失败记录,如实反映,不要编造。`

// BuildRewoo 构建 ReWOO 引擎(Reasoning Without Observation):
//
//	规划器一次调用产出完整工具计划(带 {eN} 变量引用)→ 执行器
//	按依赖就绪并行执行全部工具(LLMCompiler 式调度,无模型参与)→
//	求解器一次调用汇总证据给出回答。
//
// 全程固定两次模型调用——对比 react 每步携带全量历史调一次模型,
// token 成本大幅降低。代价是"步骤可预判"的前提:计划一次成型,不能
// 根据中间结果改道;需要边看边想的任务用 react。
//
// 本质上规划器是在运行时现场生成一张图(steps + 变量引用),与静态
// graph 的关系:一个是人写的编排,一个是模型写的编排,执行语义同源。
//
// engine_config:
//
//	max_plan_steps  计划步数上限,默认 10,超出拒绝执行
//	planner_prompt / solver_prompt  覆盖内置提示词
func BuildRewoo(ctx context.Context, asm *Assembly) (Runner, error) {
	if len(asm.Capabilities) == 0 {
		return nil, fmt.Errorf("rewoo: no tools to plan with")
	}
	tools := make(map[string]capability.Capability, len(asm.Capabilities))
	var sb strings.Builder
	for _, c := range asm.Capabilities {
		meta := c.Meta()
		tools[meta.Ref.Name] = c
		params := ""
		if meta.Params != nil {
			if js, err := meta.Params.ToJSONSchema(); err == nil && js != nil {
				if b, err := json.Marshal(js); err == nil {
					params = " 参数schema:" + string(b)
				}
			}
		}
		fmt.Fprintf(&sb, "- %s: %s%s\n", meta.Ref.Name, meta.Description, params)
	}
	return &rewooRunner{
		asm:      asm,
		tools:    tools,
		listing:  sb.String(),
		planner:  promptOr(asm, "planner", defaultRewooPlannerPrompt),
		solver:   promptOr(asm, "solver", defaultRewooSolverPrompt),
		maxSteps: asm.ConfInt("max_plan_steps", 10),
	}, nil
}

type rewooRunner struct {
	asm      *Assembly
	tools    map[string]capability.Capability
	listing  string
	planner  string
	solver   string
	maxSteps int
}

type rewooPlan struct {
	Steps []rewooStep `json:"steps"`
}

type rewooStep struct {
	ID   string         `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

var evidenceRef = regexp.MustCompile(`\{(e\d+)\}`)

func (r *rewooRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	task := renderConversation(msgs)

	// 1. 规划:一次调用产出完整计划
	out, err := r.asm.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(fmt.Sprintf("%s\n\n可用工具:\n%s\n步骤上限:%d", r.planner, r.listing, r.maxSteps)),
		schema.UserMessage("任务:\n" + task),
	})
	if err != nil {
		return nil, fmt.Errorf("rewoo plan: %w", err)
	}
	var plan rewooPlan
	if err := unmarshalLoose(out.Content, &plan); err != nil {
		return nil, fmt.Errorf("rewoo plan: parse %q: %w", out.Content, err)
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("rewoo plan: empty plan")
	}
	if len(plan.Steps) > r.maxSteps {
		return nil, fmt.Errorf("rewoo plan: %d steps exceeds limit %d", len(plan.Steps), r.maxSteps)
	}

	// 2. 执行:从 {eN} 引用推断依赖,就绪即并行(LLMCompiler 式调度)
	evidence, err := r.execute(ctx, plan.Steps)
	if err != nil {
		return nil, err
	}

	// 3. 求解:一次调用汇总证据
	var eb strings.Builder
	for _, s := range plan.Steps {
		fmt.Fprintf(&eb, "[%s] %s => %s\n", s.ID, s.Tool, evidence[s.ID])
	}
	return r.asm.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(r.solver),
		schema.UserMessage(fmt.Sprintf("任务:\n%s\n\n执行证据:\n%s", task, eb.String())),
	})
}

// execute 按依赖就绪并行执行计划。依赖从 args 字符串值里的 {eN}
// 引用推断——没有引用关系的步骤天然并行。
func (r *rewooRunner) execute(ctx context.Context, steps []rewooStep) (map[string]string, error) {
	index := map[string]int{}
	for i, s := range steps {
		if s.ID == "" || s.Tool == "" {
			return nil, fmt.Errorf("rewoo plan: step %d missing id or tool", i)
		}
		if _, dup := index[s.ID]; dup {
			return nil, fmt.Errorf("rewoo plan: duplicate step id %q", s.ID)
		}
		index[s.ID] = i
	}
	deps := make([][]int, len(steps))       // 每步依赖谁
	dependents := make([][]int, len(steps)) // 谁依赖每步
	indeg := make([]int, len(steps))
	for i, s := range steps {
		for _, ref := range collectRefs(s.Args) {
			j, ok := index[ref]
			if !ok {
				return nil, fmt.Errorf("rewoo plan: step %s references unknown {%s}", s.ID, ref)
			}
			if j >= i { // 只允许引用前序步骤,顺带排除自引用与环
				return nil, fmt.Errorf("rewoo plan: step %s references {%s} which is not an earlier step", s.ID, ref)
			}
			deps[i] = append(deps[i], j)
			dependents[j] = append(dependents[j], i)
			indeg[i]++
		}
	}

	evidence := make(map[string]string, len(steps))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var exec func(i int)
	exec = func(i int) {
		defer wg.Done()
		if ctx.Err() != nil {
			return
		}
		s := steps[i]
		mu.Lock()
		args := substituteRefs(s.Args, evidence)
		mu.Unlock()

		result := r.runTool(ctx, s.Tool, args)
		mu.Lock()
		evidence[s.ID] = result
		var next []int
		for _, j := range dependents[i] {
			if indeg[j]--; indeg[j] == 0 {
				next = append(next, j)
			}
		}
		mu.Unlock()
		for _, j := range next {
			wg.Add(1)
			go exec(j)
		}
	}
	for i, d := range indeg {
		if d == 0 {
			wg.Add(1)
			go exec(i)
		}
	}
	wg.Wait()
	return evidence, nil
}

// runTool 执行单个工具;失败以证据回传(求解器如实反映),不中断计划。
func (r *rewooRunner) runTool(ctx context.Context, name, argsJSON string) string {
	c, ok := r.tools[name]
	if !ok {
		return fmt.Sprintf("(失败:计划引用了不存在的工具 %q)", name)
	}
	out, err := capability.Invoke(ctx, c, argsJSON)
	if err != nil {
		return fmt.Sprintf("(失败:%v)", err)
	}
	return out
}

// collectRefs 收集 args 各层字符串值里的 {eN} 引用。
func collectRefs(v any) []string {
	var refs []string
	walkStrings(v, func(s string) string {
		for _, m := range evidenceRef.FindAllStringSubmatch(s, -1) {
			refs = append(refs, m[1])
		}
		return s
	})
	return refs
}

// substituteRefs 把 args 字符串值里的 {eN} 替换为对应证据,返回参数 JSON。
func substituteRefs(args map[string]any, evidence map[string]string) string {
	replaced := walkStrings(args, func(s string) string {
		return evidenceRef.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := evidence[m[1:len(m)-1]]; ok {
				return v
			}
			return m
		})
	})
	b, err := json.Marshal(replaced)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// walkStrings 深拷贝式遍历 JSON 形结构,对每个字符串值应用 fn。
func walkStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = walkStrings(val, fn)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = walkStrings(val, fn)
		}
		return out
	default:
		return v
	}
}

func (r *rewooRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return singleAsStream(r.Generate(ctx, msgs))
}
