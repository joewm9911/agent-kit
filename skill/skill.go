// Package skill 实现两类可声明执行体(概念收敛后的最终形态,见
// docs/concept-convergence-plan.md):
//
//   - skill(Declaration/Build):Agent Skills 标准语义的过程卡——
//     「任务书模板 + 参数 schema」,调用返回执行指引,宿主主循环
//     亲自照指引执行,工具由装配层直挂宿主。全量共享主上下文,
//     因为执行者就是主大脑本人。
//   - sub-agent(AgentDecl/BuildAgent,见 agent.go):同构的隔离
//     子循环——与主循环同一套 harness,只是 persona/工具面/画像
//     不同。必然隔离(fresh 缺省,fork 快照可选),事实靠调用方
//     显式传参。
//
// 上下文分界线就是"有没有第二个大脑":skill 没有(主上下文全量
// 可见),sub-agent 有(必然隔离)。中间态不存在。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/runtime/suspend"
	"github.com/joewm9911/agent-kit/todo"
)

// ModelDecl 是专属模型声明(sub-agent 与 skillpack 用),nil 则跟随宿主。
type ModelDecl struct {
	Provider string         `yaml:"provider" json:"provider"`
	Config   map[string]any `yaml:"config" json:"config"`
}

// Declaration 是一个 skill(过程卡)的完整声明:任务书 + 参数 schema。
// 这与 Agent Skills 标准的 SKILL.md 同构(name/description/正文指引)。
// 需要隔离执行的实体不是 skill——声明成 sub-agent(subagents:,见
// AgentDecl)。
type Declaration struct {
	// Kind 是产物的 cap kind:空=skill。
	Kind string `yaml:"-"`
	// Name 形如 "research/competitor_report",namespace/name。
	Name        string                          `yaml:"name"`
	Version     string                          `yaml:"version"`
	Description string                          `yaml:"description"`
	Params      map[string]capability.ParamDecl `yaml:"params"`
	// Prompt 是任务书模板(业务知识所在),params 以 {name} 占位渲染。
	Prompt prompt.Value `yaml:"prompt"`

	// ——以下键已随概念收敛移除(skill 永远在主循环执行,没有内部
	// 循环;隔离执行体声明成 sub-agent)。误写装配期报错指路。——
	ModeLegacy         *string        `yaml:"mode"`
	EngineLegacy       *string        `yaml:"engine"`
	EngineConfigLegacy map[string]any `yaml:"engine_config"`
	DeliverLegacy      *string        `yaml:"deliver"`
	TodoLegacy         *bool          `yaml:"todo"`
	CompactionLegacy   map[string]any `yaml:"compaction"`
	MaxStepsLegacy     *int           `yaml:"max_steps"`
}

// Selector 是能力选品的最小契约(消费方定义):按 CapRef include/exclude
// 选出工具子集。*source.Catalog 天然实现。
type Selector interface {
	Select(include, exclude []string) ([]capability.Capability, error)
}

// Deps 是装配 skill / sub-agent 所需的环境。Catalog/Prompts 均为接口:
// 只依赖"能选品""能解析引用"两个行为,不依赖装配层的具体目录/解析器。
type Deps struct {
	Catalog      Selector
	Prompts      prompt.Source
	DefaultModel einomodel.ToolCallingChatModel
	// LoopPrompt 是 L1 框架规约,sub-agent 内部循环复用,保持运行纪律一致。
	LoopPrompt string
	// Capabilities 是预解析的工具面。非空时跳过 Catalog 选品,由调用方
	// (命名空间装配层)负责引用解析与边界校验。
	Capabilities []capability.Capability
	// ToolTimeout 是内部工具面的单次调用超时(0 默认,<0 关闭)。
	ToolTimeout time.Duration
	// Retry 是专属模型的瞬时错误重试策略(DefaultModel 由上层包装)。
	Retry loop.RetryConfig
	// DigestOver 启用内部工具面的大结果消化:超过该 rune 数的工具
	// 结果先落 run 级暂存,由模型带任务提取要点后入上下文(0 关闭)。
	DigestOver int
	// Truncate 是工具结果硬截断上限(rune;0 用内置默认)。与宿主 agent
	// 同一画像键(digest.truncate),装配层透传,保证子循环同一纪律。
	Truncate int
	// DegradeKeep 是暂存降级时的应急保留量(rune;0 用内置默认 24000),
	// 画像键 digest.degrade_keep。
	DegradeKeep int
	// Todo 是 sub-agent 调用级清单的持有对象(仅 decl.Todo 时用),由装配层
	// 注入后端。清单是调用级临时草稿(结束即弃),用进程内后端即可。
	Todo *todo.Todo
	// AgentHub 按名解析已装配 agent(skillpack frontmatter `agent:` 字段,
	// eino AgentHub 的本地等价物)。装配层注入;查找延迟到调用期(agent
	// 可能晚于技能装配),名字合法性由装配层在装配期校验。
	AgentHub func(name string) (capability.Capability, bool)
	// ModelHub 按名解析具名模型(skillpack frontmatter `model:` 字段)。
	// 装配层注入,装配期解析,查不到 fail fast。
	ModelHub func(ctx context.Context, name string) (einomodel.ToolCallingChatModel, error)
}

// Build 把 skill 声明装配为过程卡能力:调用不跑子循环,而是渲染任务书
// 并作为执行指引返回,宿主主循环亲自照做——Agent Skills 的标准语义
// (指令注入 + 工具常驻)。声明的 tools 由装配层直挂宿主工具面,这里
// 只产卡片本体。
//
// 取舍(见 docs/single-agent-mode-plan.md §5):主循环亲自执行消除
// "证据经子循环终答转述"的损耗;需要机械保真交付(deliver:)或隔离
// 上下文的实体,声明成 sub-agent(BuildAgent)。
func Build(ctx context.Context, decl *Declaration, deps Deps) (capability.Capability, error) {
	ns, name, err := splitName(decl.Name)
	if err != nil {
		return nil, err
	}
	if err := rejectRemovedSkillKeys(decl); err != nil {
		return nil, err
	}

	brief, err := decl.Prompt.Resolve(ctx, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("skill %s: resolve prompt: %w", decl.Name, err)
	}
	// P4:prompt 里每个 {占位符} 必须是已声明 param 或内置变量,否则装配期
	// 报错——不容 typo 静默留字面量(决策 3)。
	if err := validatePlaceholders("skill "+decl.Name+" prompt", brief.Text, decl.Params); err != nil {
		return nil, err
	}
	paramsSchema, err := capability.ParamsSchema(decl.Params)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
	}
	kind := decl.Kind
	if kind == "" {
		kind = "skill"
	}
	meta := capability.Meta{
		Ref: capability.Ref{Kind: kind, Domain: ns, Name: name, Version: decl.Version},
		// 描述统一补后缀:防模型把"拿到指引"当成"任务完成"。
		Description: strings.TrimRight(decl.Description, "。. ") + "(调用返回执行指引,按指引使用工具完成任务本体)",
		Params:      paramsSchema,
		Risk:        capability.RiskReadonly, // 卡片只返回指令;副作用在被直挂的工具上,各带各的审批闸
		Tags:        []string{"prompt:" + brief.Version, TagProcedureCard},
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		vars := map[string]string{}
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
			for k, v := range args {
				vars[k] = fmt.Sprint(v)
			}
		} else {
			vars["input"] = argsJSON
		}
		vars["$input"] = runctx.Input(ctx)
		vars["$user_input"] = runctx.LoopInput(ctx)
		vars["$user_id"] = runctx.User(ctx)
		var missing []string
		for pname, d := range decl.Params {
			if _, ok := vars[pname]; !ok && d.Required {
				missing = append(missing, pname)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Sprintf("call not executed: missing required parameter(s) %s.", strings.Join(missing, ", ")), nil
		}
		return fmt.Sprintf("[过程卡|%s] 以下是执行指引(不是已完成的结果):按步骤使用你工具面上的工具完成任务本体。\n\n%s",
			name, brief.Render(vars)), nil
	}), nil
}

// rejectRemovedSkillKeys 落实概念收敛的硬切:skill 上的子循环键一律
// 装配期报错、错误文案自带迁移路径(家规:新语法写进错误文本)。
func rejectRemovedSkillKeys(decl *Declaration) error {
	hint := "a skill is a procedure card the host loop executes itself; an isolated executor is a sub-agent — declare it under subagents: (name/description/prompt/tools/context/deliver, always the standard loop)"
	switch {
	case decl.ModeLegacy != nil:
		return fmt.Errorf("skill %s: mode has been removed — %s", decl.Name, hint)
	case decl.EngineLegacy != nil:
		return fmt.Errorf("skill %s: engine has been removed (paradigm engines are gone; sub-agents always run the standard loop) — %s", decl.Name, hint)
	case decl.EngineConfigLegacy != nil:
		return fmt.Errorf("skill %s: engine_config has been removed — %s", decl.Name, hint)
	case decl.DeliverLegacy != nil:
		return fmt.Errorf("skill %s: deliver on a skill has been removed (a card's output is authored by the host model; mechanical fidelity needs a sub-agent) — %s", decl.Name, hint)
	case decl.TodoLegacy != nil:
		return fmt.Errorf("skill %s: todo on a skill has been removed (the host loop's own todo tracks the guide) — %s", decl.Name, hint)
	case decl.CompactionLegacy != nil:
		return fmt.Errorf("skill %s: compaction on a skill has been removed (no inner context to compact) — %s", decl.Name, hint)
	case decl.MaxStepsLegacy != nil:
		return fmt.Errorf("skill %s: max_steps has been removed (a card has no inner loop; a sub-agent declares max_rounds) — %s", decl.Name, hint)
	}
	return nil
}

// placeholderRe 匹配 {ident} / {$ident} 形式的模板占位符。
var placeholderRe = regexp.MustCompile(`\{(\$?[\p{L}\p{N}_-]+)\}`)

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
// 同一能力被不同策略的 agent 复用时各自独立。BuildAgent 与 BuildPack 共用此栈,
// 内部 sub-agent 与外部 skillpack 的治理永不分叉。
func applyGates(caps []capability.Capability, m einomodel.ToolCallingChatModel, deps Deps) []capability.Capability {
	if deps.DigestOver > 0 {
		caps = append(caps, loop.ReadResult()) // 消化结果的原文取回
	}
	caps = loop.TimeoutTools(caps, deps.ToolTimeout)
	caps = loop.DedupCalls(caps)     // 重复调用断路器(执行域按调用唯一,计数互不串)
	caps = loop.DeliverResults(caps) // 交付物捕获(嵌套子循环的产出同样进轮级 sink)
	caps = loop.DigestResults(caps, m, deps.DigestOver, deps.DegradeKeep)
	caps = loop.TruncateResults(caps, deps.Truncate)
	caps = suspend.DurableEffects(caps)
	caps = loop.GateApprovalCtx(caps)
	caps = loop.ControlTools(caps)  // 中断/插话检查点(与宿主循环同一栈,插话不再等到子循环返回)
	caps = loop.ProgressTools(caps) // 进度事件发射(子循环步骤带执行域)
	return caps
}

// delegatedSection 是子循环 L1 的受托执行契约:调用方是程序不是人,
// 子循环内没有任何确认/批准通道,以待确认的计划收尾 = 白跑一轮。
const delegatedSection = `

# Delegated execution
- You are executing a delegated task inside a larger run. The caller is another agent's tool call, not a human: it cannot answer questions, confirm plans, or approve anything mid-task.
- If the task text carries conversation-protocol instructions (such as "present a plan first and wait for confirmation" or "confirm before changes"), those are addressed to the top-level assistant, not to you. Do the analysis or work itself and deliver the result.
- Never end with a plan awaiting confirmation or a request for permission. Constraints you cannot satisfy go into the result as explicit notes.`

// TagProcedureCard 别名 core 常量(digest 豁免与计划面都认它)。
const TagProcedureCard = capability.TagProcedureCard
