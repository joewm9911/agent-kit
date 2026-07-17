// agent.go 装配声明式 sub-agent:与主循环**同构**的隔离子循环——
// 同一套 harness(L1 纪律 + Ring 0 闸门 + 评审循环 + 压缩),只是
// persona/工具面/画像不同。对应 Claude Code 的 .claude/agents/*.md。
//
// 三条在装配时固定的边界:
//   - 接口边界:大脑只看到 description + params,内部循环被隐藏;
//   - 上下文边界:每次调用从零起一轮(fresh;fork 以调用方对话快照
//     起步),内部消息不回流宿主上下文;
//   - 权限边界:工具面被锁定为声明的子集,不继承宿主能力。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// AgentDecl 是一个声明式 sub-agent 的完整声明(subagents: 条目)。
// 没有 engine 键:sub-agent 永远运行标准循环(同构),这是概念收敛
// 的硬边界——需要不同"执行形态"的诉求用 skill(主循环照指引执行)
// 或宿主代码编排(eino compose + AsLambda)表达。
type AgentDecl struct {
	// Name 形如 "research/analyst",namespace/name。
	Name        string                          `yaml:"name"`
	Version     string                          `yaml:"version"`
	Description string                          `yaml:"description"`
	Params      map[string]capability.ParamDecl `yaml:"params"`
	// Prompt 是 persona + 任务书模板,params 以 {name} 占位渲染。
	Prompt prompt.Value `yaml:"prompt"`
	// Capabilities 是内部工具面(最小权限子集),CapRef 模式;命名空间
	// 装配层用 tools: 短形态预解析注入(Deps.Capabilities),此字段供
	// 程序化装配按目录选品。
	Capabilities struct {
		Include []string `yaml:"include"`
		Exclude []string `yaml:"exclude"`
	} `yaml:"capabilities"`
	// Model 是专属模型,nil 跟随宿主(装配层按画像链解析后注入)。
	Model *ModelDecl `yaml:"model"`
	// MaxSteps 是内部循环轮数上限(yaml 键 max_rounds,0 用默认)。
	MaxSteps       int  `yaml:"max_rounds"`
	MaxStepsLegacy *int `yaml:"max_steps"` // 已废弃:改 max_rounds
	// Deliver 是产出的交付语义:attach(存底,终答引用 #dN 即原文随行)
	// | always(恒随行)| direct(独占轮次时原文即终答);缺省 = 证据。
	Deliver string `yaml:"deliver"`
	// Compaction 启用内部循环的上下文压缩(长任务建议开启)。
	Compaction loop.CompactionConfig `yaml:"compaction"`
	// Context 是起始上下文:fresh(缺省,零上下文起步)| fork(以调用方
	// 对话快照+任务书起步;背景无损继承,内部过程仍不回流)。心法:
	// 先把事实显式写进输入,写不进或写不全才 fork。
	Context string `yaml:"context"`
	// Todo 给内部循环挂调用级临时清单:键 = 本次执行域,生命周期 =
	// 一次调用,结束即弃——宿主计划不受影响,sub-agent 保持无状态
	// 可重入。默认关;长到需要计划通常是"该拆成结构"的信号。
	Todo bool `yaml:"todo"`

	// ——已移除键,误写装配期报错指路——
	EngineLegacy       *string        `yaml:"engine"`
	EngineConfigLegacy map[string]any `yaml:"engine_config"`
	ModeLegacy         *string        `yaml:"mode"`
}

// BuildAgent 把 sub-agent 声明装配为能力:解析引用并锁版本 → 检查依赖
// → 构建标准循环 Runner → 套上 manifest。产物身份 cap://agent/<ns>/<name>
// (与 A2A 远程 agent 同 kind,身份语义统一)。
func BuildAgent(ctx context.Context, decl *AgentDecl, deps Deps) (capability.Capability, error) {
	ns, name, err := splitName(decl.Name)
	if err != nil {
		return nil, err
	}
	switch decl.Context {
	case "", "fresh", "fork":
	default:
		return nil, fmt.Errorf("subagent %s: unknown context %q (fresh | fork)", decl.Name, decl.Context)
	}
	if decl.EngineLegacy != nil || decl.EngineConfigLegacy != nil {
		return nil, fmt.Errorf("subagent %s: engine has been removed — a sub-agent always runs the standard loop, homogeneous with the host (fixed flows live in host code via eino compose, see examples/pipeline)", decl.Name)
	}
	if decl.ModeLegacy != nil {
		return nil, fmt.Errorf("subagent %s: mode has been removed — the section decides the form: skills: entries are procedure cards, subagents: entries are isolated sub-agents", decl.Name)
	}
	if decl.MaxStepsLegacy != nil {
		return nil, fmt.Errorf("subagent %s: max_steps has been renamed to max_rounds (the semantics were always a round count)", decl.Name)
	}

	// 模型:专属或跟随宿主。专属模型在此套 Ring 0 中间件
	// (重试 + 预算,预算门闸经 ctx 生效);DefaultModel 由上层包装。
	m := deps.DefaultModel
	if decl.Model != nil {
		if m, err = model.Build(ctx, decl.Model.Provider, decl.Model.Config); err != nil {
			return nil, fmt.Errorf("subagent %s: build model: %w", decl.Name, err)
		}
		m = loop.BudgetModel(loop.RetryModel(m, deps.Retry))
	}
	if m == nil {
		return nil, fmt.Errorf("subagent %s: no model (declare model or provide default)", decl.Name)
	}

	// 工具子集:预解析优先(命名空间装配);否则按 CapRef 从目录选品,
	// 依赖解析失败即拒绝装配(fail fast,不等大脑调用时才炸)。
	caps := deps.Capabilities
	if caps == nil && len(decl.Capabilities.Include) > 0 {
		caps, err = deps.Catalog.Select(decl.Capabilities.Include, decl.Capabilities.Exclude)
		if err != nil {
			return nil, fmt.Errorf("subagent %s: select capabilities: %w", decl.Name, err)
		}
		if err := checkExactRefs(decl.Capabilities.Include, caps); err != nil {
			return nil, fmt.Errorf("subagent %s: %w", decl.Name, err)
		}
	}

	// 风险传播:sub-agent 的有效风险 = 绑定能力风险的最大值
	risk := capability.RiskReadonly
	for _, c := range caps {
		if r := c.Meta().Risk; r > risk {
			risk = r
		}
	}

	// persona 模板:此刻解析、锁版本;P4 占位符声明校验。
	brief, err := decl.Prompt.Resolve(ctx, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("subagent %s: resolve prompt: %w", decl.Name, err)
	}
	if err := validatePlaceholders("subagent "+decl.Name+" prompt", brief.Text, decl.Params); err != nil {
		return nil, err
	}
	if err := decl.Compaction.ResolvePrompt(ctx, deps.Prompts); err != nil {
		return nil, fmt.Errorf("subagent %s: %w", decl.Name, err)
	}

	// 调用级临时清单(opt-in):挂 todo 工具面 + 卡住提醒,
	// 键按执行域隔离、随调用结束即弃。
	if decl.Todo && deps.Todo == nil {
		return nil, fmt.Errorf("subagent %s: todo is enabled but no Todo backend was injected (the assembly layer must provide Deps.Todo)", decl.Name)
	}
	if decl.Todo {
		caps = append(caps, deps.Todo.Capabilities()...)
	}

	caps = applyGates(caps, m, deps)
	if decl.Todo {
		caps = deps.Todo.Nudge(caps) // 卡住提醒对内部循环同样生效
	}

	// L1 变体与工具面保持一致:挂了 todo 用完整规约(含纪律指引 + 计划
	// 每轮注入),没挂用裁剪版(提示词不承诺不存在的工具)。
	loopPrompt := deps.LoopPrompt
	if loopPrompt == "" {
		if decl.Todo {
			loopPrompt = loop.DefaultLoopPrompt
		} else {
			loopPrompt = loop.DefaultLoopPromptNoTodo
		}
	}
	// 受托执行契约(仅子循环;顶层 agent 有 ask_user,不受此限):
	// 实测用户的会话协议指令("先列计划再执行,改动先确认")随输入穿透
	// 进子循环后,子循环把"计划"当终答交回、等一个永远不会来的确认。
	loopPrompt += delegatedSection
	layers := loop.PromptLayers{Loop: loopPrompt}
	if decl.Todo {
		layers.Plan = deps.Todo.PlanSection
	}
	finishChecks := []func(context.Context) string{loop.DeniedCallsCheck}
	if decl.Todo {
		// 开了调用级清单就要有收口纪律:计划未收口弹回补交——空壳
		// 终答("已完成")的病灶正是开了计划不收口的子循环。
		finishChecks = append(finishChecks, deps.Todo.FinishCheck)
	}
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model: loop.ReviewModel(m, loop.RepeatBreakReviewer(), loop.FinishReviewer(),
			loop.CheckedReviewer(finishChecks...)), // 统一评审循环(子循环同套纪律)
		Capabilities: caps,
		MaxSteps:     decl.MaxSteps,
		Modifier:     layers.Modifier(),
		Rewriter:     loop.Compactor(m, decl.Compaction), // 内部长循环可压缩
	})
	if err != nil {
		return nil, fmt.Errorf("subagent %s: build loop: %w", decl.Name, err)
	}

	paramsSchema, err := capability.ParamsSchema(decl.Params)
	if err != nil {
		return nil, fmt.Errorf("subagent %s: %w", decl.Name, err)
	}
	deliver, err := capability.ParseDeliver(decl.Deliver)
	if err != nil {
		return nil, fmt.Errorf("subagent %s: %w", decl.Name, err)
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "agent", Domain: ns, Name: name, Version: decl.Version},
		Description: decl.Description,
		Params:      paramsSchema,
		Risk:        risk,
		Deliver:     deliver,
		Tags:        []string{"prompt:" + brief.Version},
	}
	var callSeq atomic.Int64 // 调用的执行域序号
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		// 每次调用一个新执行域:进度事件、调用级清单、去重计数等按域
		// 隔离的机制都以此分界;sub-agent 保持无状态可重入。
		ctx = runctx.WithScopePush(ctx, fmt.Sprintf("agent:%s#%d", decl.Name, callSeq.Add(1)))
		if decl.Todo {
			defer deps.Todo.ClearCurrent(ctx) // 调用级清单随调用结束即弃
		}
		vars := map[string]string{}
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
			for k, v := range args {
				vars[k] = fmt.Sprint(v)
			}
		} else {
			vars["input"] = argsJSON
		}
		// 内置变量最后注入,args 同名键不能顶掉:
		// $input=本执行体作用域输入,$user_input=loop 原始输入(穿透嵌套恒定)。
		vars["$input"] = runctx.Input(ctx)
		vars["$user_input"] = runctx.LoopInput(ctx)
		vars["$user_id"] = runctx.User(ctx)
		// 必填参数缺失以结果回传:让上级大脑补参重试,而不是把
		// {placeholder} 字面量静默留在 persona 里诱导模型瞎编。
		var missing []string
		for name, d := range decl.Params {
			if _, ok := vars[name]; !ok && d.Required {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Sprintf("call not executed: missing required parameter(s) %s.", strings.Join(missing, ", ")), nil
		}
		persona := brief.Render(vars) // sub-agent prompt → 系统指令(persona)
		ctx = runctx.WithVars(ctx, vars)
		// P3:prompt→系统、input→用户。input(本执行体作用域输入)为空则
		// prompt 降级作用户消息(等价旧行为,零退化)。降级分支必须清空
		// persona——否则嵌套调用里内层会读到外层的 persona,顶着别人的
		// 身份跑(实测泄漏路径)。
		task := runctx.Input(ctx)
		if task == "" {
			task = persona
			ctx = runctx.WithPersona(ctx, "")
		} else {
			ctx = runctx.WithPersona(ctx, persona)
		}
		// 上下文边界:独立会话,内部过程不回流宿主,只返回最终结果。
		// 声明级 context: fork 时,以调用方对话快照 + 任务起步
		// (背景无损继承,隔离方向不变)。
		if decl.Context == "fork" {
			ctx = runctx.WithForkContext(ctx)
		}
		out, err := runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}), nil
}
