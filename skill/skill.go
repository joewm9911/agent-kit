// Package skill 实现"结构进能力"的正式载体:skill 是
// 「任务书模板 + 参数 schema + 执行引擎 + 工具子集 + 可选专属模型」
// 的打包单元。打包产物实现 capability.Capability,上级大脑只看到
// 一个描述清晰的工具;内部是单次调用、小 ReAct 循环还是 plan-execute,
// 取决于声明的重量,机制完全相同。
//
// 三条在装配时固定的边界:
//   - 接口边界:大脑只看到 description + params,引擎的存在被隐藏;
//   - 上下文边界:每次调用从零起一轮,内部消息不回流宿主上下文;
//   - 权限边界:工具面被锁定为声明的子集,不继承宿主能力。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/runtime/suspend"
	"github.com/joewm9911/agent-kit/todo"
)

// ModelDecl 是 skill 专属模型声明,nil 则跟随宿主。
type ModelDecl struct {
	Provider string         `yaml:"provider" json:"provider"`
	Config   map[string]any `yaml:"config" json:"config"`
}

// Declaration 是一个 skill 的完整声明(可来自 YAML 或代码)。
type Declaration struct {
	// Kind 是产物的 cap kind:空=skill(导出成品);component 装配时置
	// "component"(私有执行单元 cap://component/<ns>/<name>)。
	Kind string `yaml:"-"`
	// Name 形如 "research/competitor_report",namespace/name。
	Name        string                          `yaml:"name"`
	Version     string                          `yaml:"version"`
	Description string                          `yaml:"description"`
	Params      map[string]capability.ParamDecl `yaml:"params"`
	// Prompt 是任务书模板(业务知识所在),params 以 {name} 占位渲染。
	Prompt prompt.Value `yaml:"prompt"`
	// Engine 是执行引擎:react(默认)| plan-execute | 已注册的自定义模板。
	Engine string `yaml:"engine"`
	// EngineConfig 透传引擎专属配置;*_prompt 键为标量(cap://prompt 前缀=引用)。
	EngineConfig map[string]any `yaml:"engine_config"`
	// Capabilities 是内部工具面(最小权限子集),CapRef 模式。
	Capabilities struct {
		Include []string `yaml:"include"`
		Exclude []string `yaml:"exclude"`
	} `yaml:"capabilities"`
	Model          *ModelDecl `yaml:"model"`
	MaxSteps       int        `yaml:"max_rounds"`
	MaxStepsLegacy *int       `yaml:"max_steps"` // 已废弃:改 max_rounds
	// Compaction 启用内部循环的上下文压缩(长任务 skill 建议开启)。
	Compaction loop.CompactionConfig `yaml:"compaction"`
	// Todo 给内部循环挂调用级临时清单(仅 react):键 = 本次执行域,
	// 生命周期 = 一次调用,结束即弃——宿主计划不受影响,组件保持无状态
	// 可重入。默认关;它是给确实拆不动的研究型长循环的例外通道,
	// component 长到需要计划通常是"该拆成结构"的信号。
	Todo bool `yaml:"todo"`
}

// Selector 是能力选品的最小契约(消费方定义):按 CapRef include/exclude
// 选出工具子集。*source.Catalog 天然实现。
type Selector interface {
	Select(include, exclude []string) ([]capability.Capability, error)
}

// Deps 是装配 skill 所需的环境。Catalog/Prompts 均为接口:skill 只依赖
// "能选品""能解析引用"两个行为,不依赖装配层的具体目录/解析器。
type Deps struct {
	Catalog      Selector
	Prompts      prompt.Source
	DefaultModel einomodel.ToolCallingChatModel
	// LoopPrompt 是 L1 框架规约,skill 内部小循环复用,保持运行纪律一致。
	LoopPrompt string
	// Capabilities 是预解析的工具面。非空时跳过 Catalog 选品,由调用方
	// (命名空间装配层)负责引用解析与边界校验。
	Capabilities []capability.Capability
	// ToolTimeout 是内部工具面的单次调用超时(0 默认,<0 关闭)。
	ToolTimeout time.Duration
	// Retry 是 skill 专属模型的瞬时错误重试策略(DefaultModel 由上层包装)。
	Retry loop.RetryConfig
	// DigestOver 启用内部工具面的大结果消化:超过该 rune 数的工具
	// 结果先落 run 级暂存,由模型带任务提取要点后入上下文(0 关闭)。
	DigestOver int
	// Todo 是组件级调用清单的持有对象(仅 decl.Todo 时用),由装配层注入
	// 后端。component 的清单是调用级临时草稿(结束即弃),用进程内后端即可。
	Todo *todo.Todo
	// AgentHub 按名解析已装配 agent(skillpack frontmatter `agent:` 字段,
	// eino AgentHub 的本地等价物)。装配层注入;查找延迟到调用期(agent
	// 可能晚于技能装配),名字合法性由装配层在装配期校验。
	AgentHub func(name string) (capability.Capability, bool)
	// ModelHub 按名解析具名模型(skillpack frontmatter `model:` 字段)。
	// 装配层注入,装配期解析,查不到 fail fast。
	ModelHub func(ctx context.Context, name string) (einomodel.ToolCallingChatModel, error)
}

// Build 把声明装配为能力:解析全部引用并锁版本 → 检查依赖 →
// 构建引擎 Runner → 套上 manifest。
func Build(ctx context.Context, decl *Declaration, deps Deps) (capability.Capability, error) {
	ns, name, err := splitName(decl.Name)
	if err != nil {
		return nil, err
	}
	engineName := decl.Engine
	if engineName == "" {
		engineName = "react"
	}

	// 模型:专属或跟随宿主。专属模型在此套 Ring 0 中间件
	// (重试 + 预算,预算门闸经 ctx 生效);DefaultModel 由上层包装。
	m := deps.DefaultModel
	if decl.Model != nil {
		if m, err = model.Build(ctx, decl.Model.Provider, decl.Model.Config); err != nil {
			return nil, fmt.Errorf("skill %s: build model: %w", decl.Name, err)
		}
		m = loop.BudgetModel(loop.RetryModel(m, deps.Retry))
	}
	if m == nil {
		return nil, fmt.Errorf("skill %s: no model (declare model or provide default)", decl.Name)
	}

	// 工具子集:预解析优先(命名空间装配);否则按 CapRef 从目录选品,
	// 依赖解析失败即拒绝装配(fail fast,不等大脑调用时才炸)。
	caps := deps.Capabilities
	if caps == nil && len(decl.Capabilities.Include) > 0 {
		caps, err = deps.Catalog.Select(decl.Capabilities.Include, decl.Capabilities.Exclude)
		if err != nil {
			return nil, fmt.Errorf("skill %s: select capabilities: %w", decl.Name, err)
		}
		if err := checkExactRefs(decl.Capabilities.Include, caps); err != nil {
			return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
		}
	}

	// 风险传播:skill 的有效风险 = 绑定能力风险的最大值
	risk := capability.RiskReadonly
	for _, c := range caps {
		if r := c.Meta().Risk; r > risk {
			risk = r
		}
	}

	// 任务书模板与引擎提示词:此刻解析、锁版本
	brief, err := decl.Prompt.Resolve(ctx, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("skill %s: resolve prompt: %w", decl.Name, err)
	}
	prompts, engineConf, err := resolveEnginePrompts(ctx, decl.EngineConfig, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
	}
	// P4:prompt/阶段提示词里每个 {占位符} 必须是已声明 param 或内置变量,
	// 否则装配期报错——不容 typo 静默留字面量(决策 3)。
	if err := validatePlaceholders("skill "+decl.Name+" prompt", brief.Text, decl.Params); err != nil {
		return nil, err
	}
	for stage, p := range prompts {
		if err := validatePlaceholders("skill "+decl.Name+" engine_config."+stage, p, decl.Params); err != nil {
			return nil, err
		}
	}
	if err := decl.Compaction.ResolvePrompt(ctx, deps.Prompts); err != nil {
		return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
	}

	// 调用级临时清单(opt-in,仅 react):挂 todo 工具面 + 卡住提醒,
	// 键按执行域隔离、随调用结束即弃。
	if decl.MaxStepsLegacy != nil {
		return nil, fmt.Errorf("skill %s: max_steps has been renamed to max_rounds (the semantics were always a round count)", decl.Name)
	}
	if decl.Todo && engineName != "react" {
		return nil, fmt.Errorf("skill %s: todo only makes sense for react (plan-execute's plan is managed by the engine, and other forms have no long loop)", decl.Name)
	}
	if decl.Todo && deps.Todo == nil {
		return nil, fmt.Errorf("skill %s: todo is enabled but no Todo backend was injected (the assembly layer must provide Deps.Todo)", decl.Name)
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
	layers := loop.PromptLayers{Loop: loopPrompt}
	if decl.Todo {
		layers.Plan = deps.Todo.PlanSection
	}
	runner, err := engine.Build(ctx, engineName, &engine.Assembly{
		Model: loop.ReviewModel(m, loop.RepeatBreakReviewer(), loop.FinishReviewer(),
			loop.CheckedReviewer(loop.DeniedCallsCheck)), // 统一评审循环(子循环同套纪律)
		Capabilities: caps,
		MaxSteps:     decl.MaxSteps,
		Modifier:     layers.Modifier(),
		Rewriter:     loop.Compactor(m, decl.Compaction), // 内部长循环可压缩
		Prompts:      prompts,
		Config:       engineConf,
	})
	if err != nil {
		return nil, fmt.Errorf("skill %s: build engine %s: %w", decl.Name, engineName, err)
	}

	kind := decl.Kind
	if kind == "" {
		kind = "skill"
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: kind, Domain: ns, Name: name, Version: decl.Version},
		Description: decl.Description,
		Params:      capability.ParamsSchema(decl.Params),
		Risk:        risk,
		Tags:        []string{"prompt:" + brief.Version},
	}
	var callSeq atomic.Int64 // 调用的执行域序号
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		// 每次调用一个新执行域:进度事件、调用级清单、去重计数等按域
		// 隔离的机制都以此分界;组件保持无状态可重入。
		ctx = runctx.WithScopePush(ctx, fmt.Sprintf("comp:%s#%d", decl.Name, callSeq.Add(1)))
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
		// 内置变量最后注入,args 同名键不能顶掉(与 graph 一致):
		// $input=本组件作用域输入,$user_input=loop 原始输入(穿透嵌套恒定)。
		vars["$input"] = runctx.Input(ctx)
		vars["$user_input"] = runctx.LoopInput(ctx)
		vars["$user_id"] = runctx.User(ctx)
		// 必填参数缺失以结果回传(与 graph 族同一语义):让上级大脑补参重试,
		// 而不是把 {placeholder} 字面量静默留在 persona 里诱导模型瞎编。
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
		persona := brief.Render(vars) // 组件 prompt → 系统指令(persona)
		// 多阶段引擎(rewoo/plan-execute/reflection)据此渲染其阶段提示词——
		// params 与内置变量透进 planner/executor/replanner 等(D1 多阶段全透)。
		ctx = runctx.WithVars(ctx, vars)
		// P3:prompt→系统、input→用户。input(本组件作用域输入)为空则 prompt
		// 降级作用户消息(等价旧行为,零退化)。降级分支必须清空 persona——
		// 否则嵌套调用里(外层组件→图→本组件,且上游 input 渲染为空)会读到
		// 外层组件的 persona,内层顶着别人的身份跑(实测泄漏路径)。
		task := runctx.Input(ctx)
		if task == "" {
			task = persona
			ctx = runctx.WithPersona(ctx, "")
		} else {
			ctx = runctx.WithPersona(ctx, persona)
		}
		// 上下文边界:独立会话,内部过程不回流宿主,只返回最终结果。
		// 使用点声明 context: fork 时,以调用方对话快照 + 任务起步
		// (背景无损继承,隔离方向不变)。
		out, err := runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}), nil
}

// placeholderRe 匹配 {ident} / {$ident} 形式的模板占位符(与 graph 引擎同款)。
var placeholderRe = regexp.MustCompile(`\{(\$?[\p{L}\p{N}_-]+)\}`)

// evidenceSyntaxRe 匹配 rewoo 的证据引用形参({e1}/{eN}),豁免 P4 声明校验。
var evidenceSyntaxRe = regexp.MustCompile(`^e(\d+|N)$`)

// validatePlaceholders 校验模板里每个占位符都是已声明 param 或内置变量,
// 否则报错(P4:prompt 不容 typo 静默留字面量;决策 3)。含非标识符字符的
// 花括号(如 JSON 示例 {"k":v})不匹配、不受约束。
func validatePlaceholders(where, text string, params map[string]capability.ParamDecl) error {
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "$") {
			switch ref {
			case "$input", "$user_input", "$user_id":
				continue
			default:
				return fmt.Errorf("%s: unknown builtin variable {%s} (allowed: $input, $user_input, $user_id)", where, ref)
			}
		}
		// input:裸串入参兜底(非 JSON 对象时整串落 {input}),合法的隐式入参。
		if ref == "input" {
			continue
		}
		// {e1}/{eN}:rewoo 证据引用语法——自定义 planner/solver 阶段提示词里
		// 合法出现(指导模型如何写引用),不是模板占位符,不受声明约束。
		if evidenceSyntaxRe.MatchString(ref) {
			continue
		}
		if _, ok := params[ref]; !ok {
			return fmt.Errorf("%s: undeclared placeholder {%s} — declare it under params: or it silently stays literal", where, ref)
		}
	}
	return nil
}

func splitName(full string) (ns, name string, err error) {
	i := strings.LastIndex(full, "/")
	if i < 0 {
		return "skills", full, nil
	}
	ns, name = full[:i], full[i+1:]
	if ns == "" || name == "" {
		return "", "", fmt.Errorf("skill: bad name %q, want namespace/name", full)
	}
	return ns, name, nil
}

// checkExactRefs 确保不含通配符的精确引用都被解析到,漏一个即失败。
func checkExactRefs(include []string, selected []capability.Capability) error {
	for _, pat := range include {
		if strings.Contains(pat, "*") {
			continue
		}
		ref, err := capability.ParseRef(pat)
		if err != nil {
			return err
		}
		found := false
		for _, c := range selected {
			if c.Meta().Ref.Match(ref) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("dependency %s not found in catalog", pat)
		}
	}
	return nil
}

// resolveEnginePrompts 把 engine_config 中 *_prompt 键解析为文本:
// 一律标量,cap://prompt 前缀 = 引用(装配期解析锁版本),其余字面量;
// 其余键原样透传给引擎。
func resolveEnginePrompts(ctx context.Context, conf map[string]any, r prompt.Source) (map[string]string, map[string]any, error) {
	prompts := map[string]string{}
	rest := map[string]any{}
	for k, v := range conf {
		if !strings.HasSuffix(k, "_prompt") {
			rest[k] = v
			continue
		}
		key := strings.TrimSuffix(k, "_prompt")
		val, ok := v.(string)
		if !ok {
			return nil, nil, fmt.Errorf(`engine_config.%s: only accepts a scalar — write references as "cap://prompt/..." (the {ref: ...} form has been removed)`, k)
		}
		if !strings.HasPrefix(val, prompt.RefPrefix) {
			prompts[key] = val
			continue
		}
		if r == nil {
			return nil, nil, fmt.Errorf("engine_config.%s: prompt ref used but no prompt sources configured", k)
		}
		tpl, err := r.Resolve(ctx, val)
		if err != nil {
			return nil, nil, err
		}
		prompts[key] = tpl.Text
	}
	return prompts, rest, nil
}

// buildDeclModel 构建专属模型并套 Ring 0 中间件(重试 + 预算门闸经 ctx)。
func buildDeclModel(ctx context.Context, decl *ModelDecl, retry loop.RetryConfig) (einomodel.ToolCallingChatModel, error) {
	m, err := model.Build(ctx, decl.Provider, decl.Config)
	if err != nil {
		return nil, fmt.Errorf("build model: %w", err)
	}
	return loop.BudgetModel(loop.RetryModel(m, retry)), nil // 质量守卫在循环装配层(ReviewModel)
}

// applyGates 给内部工具面下沉全部 Ring 0 闸门(治理不止步于 agent 主循环):
// 超时(最内,只计执行时间)→ 重复断路 → 消化 → 截断 → 效果日志 → 审批
// (最外,批准等待不占超时)。审批模式、预算门闸、结果暂存与挂起日志经调用方 ctx 生效,
// 同一能力被不同策略的 agent 复用时各自独立。Build 与 BuildPack 共用此栈,
// 内部 skill 与外部 skillpack 的治理永不分叉。
func applyGates(caps []capability.Capability, m einomodel.ToolCallingChatModel, deps Deps) []capability.Capability {
	if deps.DigestOver > 0 {
		caps = append(caps, loop.ReadResult()) // 消化结果的原文取回
	}
	caps = loop.TimeoutTools(caps, deps.ToolTimeout)
	caps = loop.DedupCalls(caps) // 重复调用断路器(执行域按调用唯一,计数互不串)
	caps = loop.DigestResults(caps, m, deps.DigestOver)
	caps = loop.TruncateResults(caps, 0)
	caps = suspend.DurableEffects(caps)
	caps = loop.GateApprovalCtx(caps)
	caps = loop.ProgressTools(caps) // 进度事件发射(子循环步骤带执行域)
	return caps
}
