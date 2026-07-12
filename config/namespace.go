// namespace.go 实现三层命名空间的装配:
//
//	namespace
//	├── tools        工具定义(mcp/http/...),ns 内共享,对外不可见
//	├── components   执行单元声明(引擎+提示词+工具子集),ns 内复用,不进全局目录
//	└── skills       对外产品(描述+参数+编排),唯一进全局目录的单元
//
// 边界规则在装配期落实:工具引用不出命名空间,components 相互引用
// 不跨命名空间,跨命名空间只能通过 cap://skill 引用(声明顺序决定
// 可见性,引用后声明的命名空间会在装配期报错)。
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/skill"
)

// nsDeps 是命名空间装配的环境。
// componentExports 是一次装配序列内的导出 component 注册表:
// 单文件形态跨全部 namespaces,多文件形态按 agent 的挂载集合——
// 与 cap://skill 的解析域同尺度。可见性按装配顺序(先装配先可见)。
type componentExports struct {
	built map[string]bool                  // 已装配完成的 ns(imports 顺序校验)
	comps map[string]capability.Capability // "<ns>/<name>" → 导出 component
}

func newComponentExports() *componentExports {
	return &componentExports{built: map[string]bool{}, comps: map[string]capability.Capability{}}
}

func (e *componentExports) add(ns, name string, c capability.Capability) error {
	key := ns + "/" + name
	if _, dup := e.comps[key]; dup {
		return fmt.Errorf("exported component %q is duplicated (was a namespace of the same name assembled twice?)", key)
	}
	e.comps[key] = c
	return nil
}

func (e *componentExports) lookup(ns, name string) (capability.Capability, bool) {
	c, ok := e.comps[ns+"/"+name]
	return c, ok
}

type nsDeps struct {
	global       *source.Catalog // skills 的落点,亦是跨 ns cap://skill 引用的解析域
	packRoot     string          // 外部 skillpack 的物化目录(.skills)
	packOpts     skill.PackOptions
	execCfg      ExecConfig // app 级默认沙箱策略(透传给 ns 内 exec 源与 pack 的 exec 工具)
	hubs         *skillHubs // frontmatter agent:/model: 的按名解析环境
	prompts      *prompt.Resolver
	defaultModel model.ToolCallingChatModel
	maxRisk      capability.Risk
	loopPrompt   string
	// base 是本 ns 之上各层合并好的执行画像(app.merge(agent自己));
	// buildNamespace 内再叠加 ns 自己的 Profile,component 再叠加自己的,
	// 最后叠加 mount 覆盖——五级就近合并。
	base Profile
	// mount 是 agent 给本 namespace 的 per-mount 覆盖画像(最高优;
	// 单文件路径为空 Profile)。
	mount Profile
	// exports 是导出 component 注册表(装配序列级,调用方创建;nil 时
	// buildNamespace 自建空表——imports 引用将查无)。
	exports *componentExports
	// appModel 是 app 层 model(判断 component 解析出的 model 是否为
	// app 默认:相同则复用共享 DefaultModel,不同则为其构建专属模型)。
	appModel *ModelConfig
	// nsPath 是 namespace 文件绝对路径,作源连接缓存键;srcCache 非 nil
	// 时同一 namespace 文件被多个 agent 实例化只建一次源连接。
	nsPath   string
	srcCache *sourceCache
	logger   *slog.Logger
}

// sourceCache 按 (namespace 文件, 源名) 缓存源的 Sync 结果,让
// namespace 的多 agent 实例化共享底层连接(MCP 等连接昂贵)。
type sourceCache struct {
	mu   sync.Mutex
	caps map[string][]capability.Capability
}

func newSourceCache() *sourceCache {
	return &sourceCache{caps: map[string][]capability.Capability{}}
}

func (c *sourceCache) get(key string) ([]capability.Capability, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	caps, ok := c.caps[key]
	return caps, ok
}

func (c *sourceCache) put(key string, caps []capability.Capability) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[key] = caps
}

// buildNamespace 装配一个命名空间:tools → components → skills,
// 只有 skills 进 deps.global(单文件路径 = 全局目录;多文件路径 =
// 每 agent 的挂载目录)。
func buildNamespace(ctx context.Context, ns *NamespaceConfig, deps nsDeps) error {
	if deps.exports == nil {
		deps.exports = newComponentExports()
	}
	// imports 顺序校验:被依赖的 ns 必须已装配(与 cap://skill 的
	// "按声明/挂载顺序可见"同一规则);顺带排除自依赖。
	imports := map[string]bool{}
	for _, imp := range ns.Imports {
		if imp == ns.Name {
			return fmt.Errorf("namespace %s: cannot import itself", ns.Name)
		}
		if !deps.exports.built[imp] {
			return fmt.Errorf("namespace %s: import %q was not assembled earlier—imports are visible in assembly/mount order, declare/mount %q before this", ns.Name, imp, imp)
		}
		imports[imp] = true
	}
	if ns.Name == "" {
		return fmt.Errorf("namespace: name is required")
	}
	// 能力不可自指 model:namespace 的执行画像不得声明 model。
	if err := ns.Profile.validateNoModel("namespace " + ns.Name); err != nil {
		return err
	}

	// 1. 工具:进 ns 本地目录,对外不可见。Sync 结果按 (ns 文件, 源名)
	// 缓存——同一 namespace 被多个 agent 实例化时源连接只建一次。
	if err := ns.Profile.rejectLegacyKeys("namespace " + ns.Name); err != nil {
		return err
	}
	if err := deps.mount.rejectLegacyKeys("namespace " + ns.Name + " (mount override)"); err != nil {
		return err
	}
	if len(ns.ToolsLegacy) > 0 {
		return fmt.Errorf("namespace %s: tools has been renamed sources (it declares capability sources, isomorphic to the top-level sources:; the tools that references the tool surface keeps its meaning)", ns.Name)
	}
	local := source.NewCatalog(deps.maxRisk, deps.logger)
	for _, tc := range ns.Sources {
		key := deps.nsPath + "|" + tc.Name
		if caps, ok := deps.srcCache.get(key); ok {
			if err := local.AddSource(ctx, source.Static(tc.Name, caps...), true, tc.Priority); err != nil {
				return fmt.Errorf("namespace %s: %w", ns.Name, err)
			}
			continue
		}
		sconf := tc.Config
		if tc.Type == "exec" {
			// app 级 exec 策略(default_sandbox/require_sandbox)同样覆盖
			// namespace 内声明的 exec 源,与顶层 sources、skillpack 一致。
			sconf = deps.execCfg.injectInto(sconf)
		}
		src, err := source.New(ctx, tc.Type, tc.Name, sconf)
		if err != nil {
			return fmt.Errorf("namespace %s: tool source %s: %w", ns.Name, tc.Name, err)
		}
		caps, err := src.Sync(ctx)
		if err != nil {
			if tc.Required {
				return fmt.Errorf("namespace %s: required source %q sync failed: %w", ns.Name, tc.Name, err)
			}
			if deps.logger != nil {
				deps.logger.Warn("optional source unavailable, skipped",
					slog.String("namespace", ns.Name), slog.String("source", tc.Name), slog.String("err", err.Error()))
			}
			continue
		}
		deps.srcCache.put(key, caps)
		if err := local.AddSource(ctx, source.Static(tc.Name, caps...), true, tc.Priority); err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
	}

	// 执行画像五级链:base = app.merge(agent),叠加 ns 自己 → nsBase;
	// nsEff = nsBase.merge(mount) 供 ns 级(skill 步骤)使用;每个 component
	// 再叠加自己的 Profile 后叠加 mount(mount 最高优),见下。
	nsBase := deps.base.merge(ns.Profile)
	nsEff := nsBase.merge(deps.mount)

	// 2. 执行单元:按声明顺序装配,产物只存在于本 ns 的构建上下文
	comps := map[string]capability.Capability{}
	for i := range ns.Components {
		cc := &ns.Components[i]
		if cc.Name == "" {
			return fmt.Errorf("namespace %s: component name is required", ns.Name)
		}
		if _, dup := comps[cc.Name]; dup {
			return fmt.Errorf("namespace %s: duplicate component %q", ns.Name, cc.Name)
		}
		// 能力不可自指 model:component 的执行画像不得声明 model。
		if err := cc.Profile.validateNoModel(fmt.Sprintf("namespace %s: component %s", ns.Name, cc.Name)); err != nil {
			return err
		}
		if err := cc.Profile.rejectLegacyKeys(fmt.Sprintf("namespace %s: component %s", ns.Name, cc.Name)); err != nil {
			return err
		}
		// component 生效画像:nsBase.merge(自己) 再叠加 mount(最高优)。
		// 五级就近合并:mount > component > namespace > agent > app。
		eff := nsBase.merge(cc.Profile).merge(deps.mount)

		// 编排族:steps 声明 → 私有的无脑图(graph/workflow)
		if len(cc.Steps) > 0 {
			c, err := buildGraphComponent(ctx, ns.Name, cc, local, comps, imports, deps, eff)
			if err != nil {
				return fmt.Errorf("namespace %s: component %s: %w", ns.Name, cc.Name, err)
			}
			comps[cc.Name] = c
			if cc.Export {
				if err := deps.exports.add(ns.Name, cc.Name, c); err != nil {
					return fmt.Errorf("namespace %s: %w", ns.Name, err)
				}
			}
			continue
		}

		// 循环族:engine 同样必填——执行形态决定成本模型(direct 1~2 次
		// 调用,react N 次,plan-execute N×M 次),不做隐式默认。
		switch cc.Engine {
		case "":
			return fmt.Errorf("namespace %s: component %s: engine must be declared explicitly: direct (single shot) | react (loop) | plan-execute (planning loop) | reflection | router (triage) | rewoo (plan once, execute in parallel) | a registered template", ns.Name, cc.Name)
		case "graph", "workflow":
			return fmt.Errorf("namespace %s: component %s: engine %s requires a steps declaration (orchestration family)", ns.Name, cc.Name, cc.Engine)
		}
		caps, err := resolveToolFace(ns.Name, cc.Tools, local, comps, imports, deps.exports, deps.global)
		if err != nil {
			return fmt.Errorf("namespace %s: component %s: %w", ns.Name, cc.Name, err)
		}
		decl := &skill.Declaration{
			Kind:         "component",
			Deliver:      cc.Deliver,
			Name:         ns.Name + "/" + cc.Name,
			Prompt:       cc.Prompt,
			Params:       cc.Params, // 循环族 component 的入参声明:工具面 schema + P4 占位符校验
			Engine:       cc.Engine,
			EngineConfig: cc.EngineConfig,
			MaxSteps:     eff.maxSteps(),
			Compaction:   eff.compaction(),
			Todo:         cc.Todo,
		}
		// model 走执行画像三级链(mount > agent > app;ns/component 不可自指)。
		// 解析出的 model 与 app 默认相同 → 复用共享 DefaultModel(不重建);
		// 不同(agent/mount 指定)→ 为其构建专属模型。
		if eff.Model != nil && eff.Model != deps.appModel {
			decl.Model = &skill.ModelDecl{Provider: eff.Model.Provider, Config: eff.Model.Config}
		} else if cc.Profile.Reliability.Retry != nil && deps.logger != nil {
			// 重试是模型中间件:复用共享模型时,该模型携带的是 app 层
			// 重试,component 声明的 retry 装不上——静默无效不如说清楚。
			deps.logger.Warn("component reliability.retry only takes effect with a dedicated model (mount/agent-specified); the shared default model carries the app-level retry",
				"namespace", ns.Name, "component", cc.Name)
		}
		c, err := skill.Build(ctx, decl, skill.Deps{
			Todo:         componentTodo(),
			Prompts:      deps.prompts,
			DefaultModel: deps.defaultModel,
			LoopPrompt:   deps.loopPrompt,
			Capabilities: caps,
			ToolTimeout:  eff.toolTimeout().Std(),
			Retry:        eff.retry(),
			DigestOver:   eff.digestOver(),
			Truncate:     eff.digestTruncate(),
		})
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		comps[cc.Name] = c
		if cc.Export {
			if err := deps.exports.add(ns.Name, cc.Name, c); err != nil {
				return fmt.Errorf("namespace %s: %w", ns.Name, err)
			}
		}
	}

	// 3. skills:编排引用 → 编译为 DAG → 进 deps.global。
	// use: 外部链接是第四形态(skillpack):物化 → 校验 → 隔离子循环。
	for i := range ns.Skills {
		sc := &ns.Skills[i]
		if sc.Use != "" && isExternalRef(sc.Use) {
			return fmt.Errorf(`namespace %s: skill %s: external links now use from (use only keeps capability-reference semantics): from: %q`, ns.Name, sc.Name, sc.Use)
		}
		if sc.From != "" {
			if !isExternalRef(sc.From) {
				return fmt.Errorf("namespace %s: skill %s: from only accepts external links (github.com/...|https://...|file:...); use use for internal references", ns.Name, sc.Name)
			}
			if len(sc.Steps) > 0 {
				return fmt.Errorf("namespace %s: skill %s: from and steps are mutually exclusive", ns.Name, sc.Name)
			}
			if sc.Name == "" {
				return fmt.Errorf("namespace %s: external skillpack (%s) must have an explicit name (to name the owning team)", ns.Name, sc.From)
			}
			c, err := buildSkillpack(ctx, deps.packRoot, deps.packOpts,
				skill.PackSpec{Use: sc.From, Integrity: sc.Integrity, Name: ns.Name + "/" + sc.Name},
				skill.PackOverrides{MaxSteps: sc.MaxRounds, Tools: sc.Tools, Context: sc.Context},
				skill.Deps{Catalog: deps.global, Prompts: deps.prompts,
					DefaultModel: deps.defaultModel, Retry: nsEff.retry(),
					ToolTimeout: nsEff.toolTimeout().Std(), DigestOver: nsEff.digestOver(),
					Truncate: nsEff.digestTruncate()},
				deps.execCfg, deps.hubs)
			if err != nil {
				return fmt.Errorf("namespace %s: %w", ns.Name, err)
			}
			if err := deps.global.Add(c); err != nil {
				return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
			}
			continue
		}
		if sc.MaxRounds != 0 {
			return fmt.Errorf("namespace %s: skill %s: max_rounds only applies to from (external skillpack) skills; orchestration skills have no inner loop (per-step limits live on the target component)", ns.Name, sc.Name)
		}
		steps := sc.Steps
		if sc.Use != "" { // 入口引用形态:单步透传,skill 只是接口
			if len(steps) > 0 {
				return fmt.Errorf("namespace %s: skill %s: use and steps are mutually exclusive", ns.Name, sc.Name)
			}
			if sc.Engine != "" {
				return fmt.Errorf("namespace %s: skill %s: engine only pairs with steps (use is pure delegation)", ns.Name, sc.Name)
			}
			steps = []engine.Step{{Name: "main", Use: sc.Use}}
		}
		eng := sc.Engine
		if eng == "" {
			eng = "graph" // skill 只有编排一族,缺省 DAG 全形态
		}
		if err := validateStepsEngine(eng, steps); err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
		resolver := stepResolver(ns.Name, local, comps, imports, deps.exports, deps.global, deps.defaultModel)
		steps, err := resolveStepArgs(ctx, steps, deps.prompts)
		if err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
		c, err := engine.BuildGraph(ctx, &engine.GraphDeclaration{
			Name: sc.Name, Version: sc.Version, Description: sc.Description,
			Deliver: sc.Deliver,
			Params: sc.Params, Output: sc.Output,
			Steps: applyStepDefaults(steps, sc.StepDefaults.Timeout, sc.StepDefaults.Retry, nsEff.stepTimeout(), nsEff.stepRetry()),
		}, ns.Name, resolver)
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		if err := deps.global.Add(c); err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
	}
	deps.exports.built[ns.Name] = true
	return nil
}

// resolveStepArgs 在装配期把每个步骤的 prompt/args 收敛为引擎消费的
// 字面量模板(fail fast 全在这里,引擎零策略):
//   - model 步骤:prompt 必填(cap://prompt/ 前缀经提示词源解析锁版本),
//     args 参数映射绑定进模板占位符(键不存在报错),收敛为字面量;
//   - 工具/component 步骤:不得有 prompt;args 参数映射逐值保位拼为
//     JSON 对象模板(值里的 {占位} 留给运行时渲染)。
func resolveStepArgs(ctx context.Context, steps []engine.Step, prompts *prompt.Resolver) ([]engine.Step, error) {
	out := make([]engine.Step, len(steps))
	for i, s := range steps {
		if s.Use == "model" {
			if s.Prompt.IsZero() {
				return nil, fmt.Errorf(`step %q: use: model requires prompt: (the prompt; args only holds parameter bindings)`, s.Name)
			}
			if s.Args.Literal != "" {
				return nil, fmt.Errorf(`step %q: a model step writes its prompt in prompt:, args only accepts a parameter mapping`, s.Name)
			}
			tpl, err := s.Prompt.Resolve(ctx, prompts)
			if err != nil {
				return nil, fmt.Errorf("step %q: prompt %w", s.Name, err)
			}
			text := tpl.Text
			// 参数绑定:键必须是模板里存在的占位符(写了就必须有效);
			// 值里的 {步骤}/{参数}/{$input} 留给运行时渲染。
			for k, v := range s.Args.Fields {
				ph := "{" + k + "}"
				if !strings.Contains(text, ph) {
					return nil, fmt.Errorf("step %q: parameter %q has no matching placeholder {%s} in the prompt template", s.Name, k, k)
				}
				text = strings.ReplaceAll(text, ph, v)
			}
			s.Prompt = prompt.Value{}
			s.Args = engine.StepArgs{Literal: text}
		} else {
			if !s.Prompt.IsZero() {
				return nil, fmt.Errorf(`step %q: prompt is only for use: model steps (tool/component inputs go in args)`, s.Name)
			}
			if s.Args.Fields != nil {
				b, err := json.Marshal(s.Args.Fields)
				if err != nil {
					return nil, fmt.Errorf("step %q: args: %w", s.Name, err)
				}
				s.Args = engine.StepArgs{Literal: string(b)}
			}
		}
		out[i] = s
	}
	return out, nil
}

// applyStepDefaults 应用步骤参数的 override 链:步骤显式声明 →
// skill 的 step_defaults(sdTimeout/sdRetry)→ 执行画像 steps 默认
// (defTimeout/defRetry,已含 mount/ns/agent/app 就近合并)。
// 0 视为未声明,负值表示显式关闭(retry: -1 = 不重试,即便上层有默认)。
func applyStepDefaults(steps []engine.Step, sdTimeout loop.Duration, sdRetry int, defTimeout loop.Duration, defRetry int) []engine.Step {
	if sdTimeout == 0 {
		sdTimeout = defTimeout
	}
	if sdRetry == 0 {
		sdRetry = defRetry
	}
	if sdTimeout == 0 && sdRetry == 0 {
		return steps
	}
	out := make([]engine.Step, len(steps))
	for i, s := range steps {
		if s.Timeout == 0 {
			s.Timeout = sdTimeout
		}
		if s.Retry == 0 {
			s.Retry = sdRetry
		}
		out[i] = s
	}
	return out
}

// buildGraphComponent 装配编排族 component:steps 复用 skill 的图
// 执行器,产物只进本 ns 的 comps 表(私有,不进目录)。skill 与它的
// 区别只剩可见性——skill 是导出的图,这里是私有的图。
func buildGraphComponent(ctx context.Context, nsName string, cc *ComponentConfig,
	local *source.Catalog, comps map[string]capability.Capability, imports map[string]bool,
	deps nsDeps, eff Profile) (capability.Capability, error) {

	if !cc.Prompt.IsZero() || len(cc.Tools) > 0 || cc.EngineConfig != nil || cc.Loop.MaxSteps != nil || cc.Todo {
		return nil, fmt.Errorf("steps is mutually exclusive with prompt/tools/engine_config/max_steps/todo (the orchestration family has no brain; the plan is the steps themselves)")
	}
	if cc.Engine == "" {
		return nil, fmt.Errorf("engine must be declared explicitly: graph (DAG, can run in parallel) | workflow (strictly sequential)—the execution shape is the fact a config reader most needs to see at a glance")
	}
	if err := validateStepsEngine(cc.Engine, cc.Steps); err != nil {
		return nil, err
	}
	resolver := stepResolver(nsName, local, comps, imports, deps.exports, deps.global, deps.defaultModel)
	steps, err := resolveStepArgs(ctx, cc.Steps, deps.prompts)
	if err != nil {
		return nil, err
	}
	return engine.BuildGraph(ctx, &engine.GraphDeclaration{
		Kind: "component", Deliver: cc.Deliver,
		Name: cc.Name, Params: cc.Params,
		Steps:  applyStepDefaults(steps, 0, 0, eff.stepTimeout(), eff.stepRetry()),
		Output: cc.Output,
	}, nsName, resolver)
}

// validateStepsEngine 校验编排形态词汇(component 与 ns skill 共用):
// graph 是 DAG 全形态;workflow 是顺序简化形态,禁显式 needs。
func validateStepsEngine(eng string, steps []engine.Step) error {
	switch eng {
	case "graph":
	case "workflow":
		for _, s := range steps {
			if s.Needs != nil {
				return fmt.Errorf("step %q: workflow is the simplified sequential form and does not support needs (for a DAG use engine: graph)", s.Name)
			}
		}
	default:
		return fmt.Errorf("steps can only pair with engine: graph|workflow, got %q", eng)
	}
	return nil
}

// resolveRef 是工具面与编排步共用的单引用解析内核(#6 合流)。同一套
// 引用词汇(tools/ · components/ · model · cap://)在一处解析;两个调用
// 点的差异降为参数:wildcardOK 控制 tools/ 是否允许通配(工具面允许、
// 编排步要求精确),m 提供 use: model 的模型(工具面传 nil)。
//
// tools/ 通配可返回多个能力,其余恰返回一个;命中 0 个报错。
func resolveRef(nsName, ref string, local, global *source.Catalog,
	comps map[string]capability.Capability, imports map[string]bool, exports *componentExports,
	m model.ToolCallingChatModel, wildcardOK bool) ([]capability.Capability, error) {

	switch {
	case ref == "model":
		if m == nil {
			return nil, fmt.Errorf("step uses model but no default_model configured")
		}
		return []capability.Capability{modelStepCap(m)}, nil
	case strings.HasPrefix(ref, "tools/"):
		if !wildcardOK && strings.Contains(ref, "*") {
			return nil, fmt.Errorf("step reference %q must be exact (no wildcard)", ref)
		}
		pattern, err := toolPattern(ref)
		if err != nil {
			return nil, err
		}
		caps, err := local.Select([]string{pattern}, nil)
		if err != nil {
			return nil, err
		}
		if len(caps) == 0 {
			return nil, fmt.Errorf("%s matches no tool in this namespace (tools do not cross namespaces)", ref)
		}
		return caps, nil
	case strings.HasPrefix(ref, "components/"):
		name := strings.TrimPrefix(ref, "components/")
		c, ok := comps[name]
		if !ok {
			return nil, fmt.Errorf("component %q not declared (yet) in namespace %s", name, nsName)
		}
		return []capability.Capability{c}, nil
	case strings.HasPrefix(ref, "cap://component/"):
		// 跨 ns 导出 component:必须显式 imports + 对方显式 export,
		// 全称引用(不落本地短名空间,来源在使用点一眼可见)。
		rest := strings.TrimPrefix(ref, "cap://component/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("bad reference %q: want cap://component/<ns>/<name>", ref)
		}
		depNS, depName := parts[0], parts[1]
		if !imports[depNS] {
			return nil, fmt.Errorf("%s: namespace %s does not declare imports: [%s] (dependencies must be explicit)", ref, nsName, depNS)
		}
		if exports == nil {
			return nil, fmt.Errorf("%s: no export registry (the assembly sequence did not enable imports)", ref)
		}
		c, ok := exports.lookup(depNS, depName)
		if !ok {
			return nil, fmt.Errorf("%s: namespace %s does not export component %q (declare export: true on it)", ref, depNS, depName)
		}
		return []capability.Capability{c}, nil
	case strings.HasPrefix(ref, "cap://"):
		c, err := crossNamespaceSkill(ref, global)
		if err != nil {
			return nil, err
		}
		return []capability.Capability{c}, nil
	default:
		return nil, fmt.Errorf("bad reference %q: want tools/<source>/<name>, components/<name>, model, cap://skill... or cap://component/<ns>/<name>", ref)
	}
}

// resolveToolFace 解析 component 的工具面引用(允许 tools/ 通配,批量展开)。
func resolveToolFace(nsName string, refs []string, local *source.Catalog,
	comps map[string]capability.Capability, imports map[string]bool, exports *componentExports,
	global *source.Catalog) ([]capability.Capability, error) {

	var out []capability.Capability
	for _, ref := range refs {
		caps, err := resolveRef(nsName, ref, local, global, comps, imports, exports, nil, true)
		if err != nil {
			return nil, err
		}
		out = append(out, caps...)
	}
	return out, nil
}

// stepResolver 返回编排步骤的引用解析器(装配期调用):要求精确单一命中。
func stepResolver(nsName string, local *source.Catalog, comps map[string]capability.Capability,
	imports map[string]bool, exports *componentExports,
	global *source.Catalog, m model.ToolCallingChatModel) engine.StepResolver {

	return func(use string) (capability.Capability, error) {
		caps, err := resolveRef(nsName, use, local, global, comps, imports, exports, m, false)
		if err != nil {
			return nil, err
		}
		if len(caps) != 1 {
			return nil, fmt.Errorf("%s is ambiguous (%d matches)", use, len(caps))
		}
		return caps[0], nil
	}
}

// toolPattern 把 tools/<source>/<name> 翻译为本地目录的选品模式。
// kind 写死 tool(通配不变式:kind 精确),domain=源名、name 原样(可含
// name 段通配)。
func toolPattern(ref string) (string, error) {
	rest := strings.TrimPrefix(ref, "tools/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("bad tool reference %q: want tools/<source>/<name>", ref)
	}
	return fmt.Sprintf("cap://tool/%s/%s", parts[0], parts[1]), nil
}

// crossNamespaceSkill 落实跨命名空间边界:cap:// 全名引用只允许
// kind=skill——skill 是命名空间的唯一公开接口。
func crossNamespaceSkill(refStr string, global *source.Catalog) (capability.Capability, error) {
	ref, err := capability.ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	if ref.Kind != "skill" {
		return nil, fmt.Errorf("%s: only cap://skill refs may cross namespaces (tools and components do not leave the namespace)", refStr)
	}
	return global.Get(refStr)
}

// modelStepCap 把默认模型包装为 use: model 步骤的能力:单次调用。
// P3:prompt(=args,params 已绑定+运行时渲染)→ 系统消息;input(P2 re-scope
// 的作用域输入)→ 用户消息;input 为空则 prompt 降级作用户消息(零退化)。
// 步骤声明 context: fork 时,以调用方对话快照起步。
func modelStepCap(m model.ToolCallingChatModel) capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "model_step"},
		Description: "单次模型调用",
		Risk:        capability.RiskReadonly, // 纯模型调用,无外部副作用
	}, func(ctx context.Context, args string) (string, error) {
		system, user := args, runctx.Input(ctx)
		var msgs []*schema.Message
		if user == "" {
			msgs = loop.ForkMessages(ctx, schema.UserMessage(system)) // 兜底:prompt 作用户消息
		} else {
			msgs = append([]*schema.Message{schema.SystemMessage(system)},
				loop.ForkMessages(ctx, schema.UserMessage(user))...)
		}
		out, err := m.Generate(ctx, msgs)
		if err != nil {
			return "", err
		}
		return out.Content, nil
	})
}
