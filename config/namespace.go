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
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/skill"
	"github.com/joewm9911/agent-kit/source"
)

// ComponentConfig 声明一个执行单元:能力声明与能力使用分离的"声明"侧。
// 不进全局目录、对外不可见。engine **必填**——执行形态决定成本模型与
// 行为保证,是读配置的人最需要一眼看到的事实,不做隐式默认:
//
//	循环族(prompt + tools):engine = direct(单发:一次调用+一轮工具+
//	  收尾,无循环)| react(自主循环)| plan-execute(规划循环)| 已注册模板;
//	编排族(steps):engine = graph(DAG,可并行)| workflow(纯顺序,
//	  禁 needs)。无脑钉死序列,复用 skill 的图执行器,params 显式化
//	  入参契约。两族字段互斥。
type ComponentConfig struct {
	Name   string       `yaml:"name"`
	Engine string       `yaml:"engine"`
	Prompt prompt.Value `yaml:"prompt"`
	// Tools 是循环族的工具面引用:tools/<source>/<name|*>(本 ns 工具)、
	// components/<name>(本 ns 执行单元)、cap://skill 引用(跨 ns)。
	Tools        []string              `yaml:"tools"`
	Model        *ModelConfig          `yaml:"model"`
	MaxSteps     int                   `yaml:"max_steps"`
	EngineConfig map[string]any        `yaml:"engine_config"`
	Compaction   loop.CompactionConfig `yaml:"compaction"`
	// DigestOver 启用内部工具面的大结果消化(0 = 未声明,走 defaults 链)。
	DigestOver int `yaml:"digest_over"`

	// 编排族:私有的无脑序列/图(字段语义同 skill 的对应项)。
	Params map[string]skill.ParamDecl `yaml:"params"`
	Steps  []skill.Step               `yaml:"steps"`
	Output string                     `yaml:"output"`
}

// NamespaceSkill 声明一个对外 skill:接口(描述+参数)+ 编排(steps,
// 纯引用)。steps 的语义是 DAG,见 skill.Step。
type NamespaceSkill struct {
	Name        string                     `yaml:"name"`
	Version     string                     `yaml:"version"`
	Description string                     `yaml:"description"`
	Params      map[string]skill.ParamDecl `yaml:"params"`
	Steps       []skill.Step               `yaml:"steps"`
	Output      string                     `yaml:"output"`
	// Use 是入口引用形态(与 steps 互斥):skill 退化为纯接口声明
	// (description + params),执行整体委托给一个 component
	// (通常是 graph/workflow 形态),params JSON 原样透传。
	Use string `yaml:"use"`
	// StepDefaults 是本 skill 步骤未声明 timeout/retry 时的缺省
	// (override 链的 skill 层;更下层的步骤显式声明优先)。
	StepDefaults struct {
		Timeout loop.Duration `yaml:"timeout"`
		Retry   int           `yaml:"retry"`
	} `yaml:"step_defaults"`
}

// NamespaceConfig 是一个配置命名空间的完整声明。
type NamespaceConfig struct {
	Name       string            `yaml:"name"`
	Tools      []SourceConfig    `yaml:"tools"`
	Components []ComponentConfig `yaml:"components"`
	Skills     []NamespaceSkill  `yaml:"skills"`
}

// nsDeps 是命名空间装配的环境。
type nsDeps struct {
	global       *source.Catalog // skills 的落点,亦是跨 ns cap://skill 引用的解析域
	prompts      *prompt.Resolver
	defaultModel model.ToolCallingChatModel
	maxRisk      capability.Risk
	loopPrompt   string
	toolTimeout  loop.Duration
	retry        loop.RetryConfig
	// defaults 是本 ns 之上各层合并好的执行参数默认值(agent 级已并入;
	// buildNamespace 内再叠加 ns 自己的 defaults,就近优先)。
	defaults Defaults
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
	if ns.Name == "" {
		return fmt.Errorf("namespace: name is required")
	}

	// 1. 工具:进 ns 本地目录,对外不可见。Sync 结果按 (ns 文件, 源名)
	// 缓存——同一 namespace 被多个 agent 实例化时源连接只建一次。
	local := source.NewCatalog(deps.maxRisk, deps.logger)
	for _, tc := range ns.Tools {
		key := deps.nsPath + "|" + tc.Name
		if caps, ok := deps.srcCache.get(key); ok {
			if err := local.AddSource(ctx, source.Static(tc.Name, caps...), true, tc.Priority); err != nil {
				return fmt.Errorf("namespace %s: %w", ns.Name, err)
			}
			continue
		}
		src, err := source.New(ctx, tc.Type, tc.Name, tc.Config)
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

	// 执行参数 override 链:component 显式声明 → ns/agent 合并默认
	// (deps.defaults 已含 agent 层,ns 层由 BuildApp 并入)→ app 全局。
	eff := deps.defaults

	toolTimeout := deps.toolTimeout
	if eff.ToolTimeout != nil {
		toolTimeout = *eff.ToolTimeout
	}
	retry := deps.retry
	if eff.Retry != nil {
		retry = *eff.Retry
	}

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

		// 编排族:steps 声明 → 私有的无脑图(graph/workflow)
		if len(cc.Steps) > 0 {
			c, err := buildGraphComponent(ctx, ns.Name, cc, local, comps, deps, eff)
			if err != nil {
				return fmt.Errorf("namespace %s: component %s: %w", ns.Name, cc.Name, err)
			}
			comps[cc.Name] = c
			continue
		}

		// 循环族:engine 同样必填——执行形态决定成本模型(direct 1~2 次
		// 调用,react N 次,plan-execute N×M 次),不做隐式默认。
		switch cc.Engine {
		case "":
			return fmt.Errorf("namespace %s: component %s: engine 必须显式声明:direct(单发)| react(循环)| plan-execute(规划循环)| reflection(反思)| router(分诊)| rewoo(一次规划并行执行)| 已注册模板", ns.Name, cc.Name)
		case "graph", "workflow":
			return fmt.Errorf("namespace %s: component %s: engine %s 需要 steps 声明(编排族)", ns.Name, cc.Name, cc.Engine)
		}
		caps, err := resolveToolFace(ns.Name, cc.Tools, local, comps, deps.global)
		if err != nil {
			return fmt.Errorf("namespace %s: component %s: %w", ns.Name, cc.Name, err)
		}
		decl := &skill.Declaration{
			Name:         ns.Name + "/" + cc.Name,
			Prompt:       cc.Prompt,
			Engine:       cc.Engine,
			EngineConfig: cc.EngineConfig,
			MaxSteps:     cc.MaxSteps,
			Compaction:   cc.Compaction,
		}
		// component 未显式声明的执行参数,从 defaults 链取
		if decl.MaxSteps == 0 && eff.MaxSteps != nil {
			decl.MaxSteps = *eff.MaxSteps
		}
		if !decl.Compaction.Enabled() && eff.Compaction != nil {
			decl.Compaction = *eff.Compaction
		}
		mc := cc.Model
		if mc == nil {
			mc = eff.Model
		}
		if mc != nil {
			decl.Model = &skill.ModelDecl{Provider: mc.Provider, Config: mc.Config}
		}
		digestOver := cc.DigestOver
		if digestOver == 0 && eff.DigestOver != nil {
			digestOver = *eff.DigestOver
		}
		c, err := skill.Build(ctx, decl, skill.Deps{
			Prompts:      deps.prompts,
			DefaultModel: deps.defaultModel,
			LoopPrompt:   deps.loopPrompt,
			Capabilities: caps,
			ToolTimeout:  toolTimeout.Std(),
			Retry:        retry,
			DigestOver:   digestOver,
		})
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		comps[cc.Name] = c
	}

	// 3. skills:编排引用 → 编译为 DAG → 进 deps.global
	for i := range ns.Skills {
		sc := &ns.Skills[i]
		steps := sc.Steps
		if sc.Use != "" { // 入口引用形态:单步透传,skill 只是接口
			if len(steps) > 0 {
				return fmt.Errorf("namespace %s: skill %s: use 与 steps 互斥", ns.Name, sc.Name)
			}
			steps = []skill.Step{{Name: "main", Use: sc.Use}}
		}
		resolver := stepResolver(ns.Name, local, comps, deps.global, deps.defaultModel)
		c, err := skill.BuildGraph(ctx, &skill.GraphDeclaration{
			Name: sc.Name, Version: sc.Version, Description: sc.Description,
			Params: sc.Params, Output: sc.Output,
			Steps: applyStepDefaults(steps, sc.StepDefaults.Timeout, sc.StepDefaults.Retry, eff),
		}, ns.Name, resolver)
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		if err := deps.global.Add(c); err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
	}
	return nil
}

// applyStepDefaults 应用步骤参数的 override 链:步骤显式声明 →
// skill 的 step_defaults(sdTimeout/sdRetry)→ ns/agent 合并默认。
// 0 视为未声明,负值表示显式关闭(retry: -1 = 不重试,即便上层有默认)。
func applyStepDefaults(steps []skill.Step, sdTimeout loop.Duration, sdRetry int, eff Defaults) []skill.Step {
	if sdTimeout == 0 && eff.StepTimeout != nil {
		sdTimeout = *eff.StepTimeout
	}
	if sdRetry == 0 && eff.StepRetry != nil {
		sdRetry = *eff.StepRetry
	}
	if sdTimeout == 0 && sdRetry == 0 {
		return steps
	}
	out := make([]skill.Step, len(steps))
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
	local *source.Catalog, comps map[string]capability.Capability, deps nsDeps, eff Defaults) (capability.Capability, error) {

	if !cc.Prompt.IsZero() || len(cc.Tools) > 0 || cc.EngineConfig != nil || cc.MaxSteps > 0 {
		return nil, fmt.Errorf("steps 与 prompt/tools/engine_config/max_steps 互斥(编排族没有大脑)")
	}
	switch cc.Engine {
	case "graph":
	case "workflow": // 顺序简化形态:只允许缺省的"依赖上一步"链
		for _, s := range cc.Steps {
			if s.Needs != nil {
				return nil, fmt.Errorf("step %q: workflow 是顺序简化形态,不支持 needs(要 DAG 用 engine: graph)", s.Name)
			}
		}
	case "":
		return nil, fmt.Errorf("engine 必须显式声明:graph(DAG,可并行)| workflow(纯顺序)——执行形态是读配置的人最需要一眼看到的事实")
	default:
		return nil, fmt.Errorf("steps 只能与 engine: graph|workflow 搭配,当前 %q", cc.Engine)
	}
	resolver := stepResolver(nsName, local, comps, deps.global, deps.defaultModel)
	return skill.BuildGraph(ctx, &skill.GraphDeclaration{
		Name: cc.Name, Params: cc.Params,
		Steps:  applyStepDefaults(cc.Steps, 0, 0, eff),
		Output: cc.Output,
	}, nsName, resolver)
}

// resolveToolFace 解析 component 的工具面引用并落实边界规则。
func resolveToolFace(nsName string, refs []string, local *source.Catalog,
	comps map[string]capability.Capability, global *source.Catalog) ([]capability.Capability, error) {

	var out []capability.Capability
	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref, "tools/"):
			pattern, err := toolPattern(ref)
			if err != nil {
				return nil, err
			}
			caps, err := local.Select([]string{pattern}, nil)
			if err != nil {
				return nil, err
			}
			if len(caps) == 0 {
				return nil, fmt.Errorf("%s matches no tool in this namespace (工具不跨命名空间)", ref)
			}
			out = append(out, caps...)
		case strings.HasPrefix(ref, "components/"):
			name := strings.TrimPrefix(ref, "components/")
			c, ok := comps[name]
			if !ok {
				return nil, fmt.Errorf("component %q not declared (yet) in namespace %s", name, nsName)
			}
			out = append(out, c)
		case strings.HasPrefix(ref, "cap://"):
			c, err := crossNamespaceSkill(ref, global)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("bad reference %q: want tools/<source>/<name>, components/<name> or cap://skill...", ref)
		}
	}
	return out, nil
}

// stepResolver 返回编排步骤的引用解析器(装配期调用)。
func stepResolver(nsName string, local *source.Catalog, comps map[string]capability.Capability,
	global *source.Catalog, m model.ToolCallingChatModel) skill.StepResolver {

	return func(use string) (capability.Capability, error) {
		switch {
		case use == "model":
			if m == nil {
				return nil, fmt.Errorf("step uses model but no default_model configured")
			}
			return modelStepCap(m), nil
		case strings.HasPrefix(use, "components/"):
			name := strings.TrimPrefix(use, "components/")
			c, ok := comps[name]
			if !ok {
				return nil, fmt.Errorf("component %q not declared in namespace %s", name, nsName)
			}
			return c, nil
		case strings.HasPrefix(use, "tools/"):
			if strings.Contains(use, "*") {
				return nil, fmt.Errorf("step reference %q must be exact (no wildcard)", use)
			}
			pattern, err := toolPattern(use)
			if err != nil {
				return nil, err
			}
			caps, err := local.Select([]string{pattern}, nil)
			if err != nil {
				return nil, err
			}
			switch len(caps) {
			case 0:
				return nil, fmt.Errorf("%s matches no tool in this namespace (工具不跨命名空间)", use)
			case 1:
				return caps[0], nil
			default:
				return nil, fmt.Errorf("%s is ambiguous (%d matches)", use, len(caps))
			}
		case strings.HasPrefix(use, "cap://"):
			return crossNamespaceSkill(use, global)
		default:
			return nil, fmt.Errorf("bad use %q: want components/<name>, tools/<source>/<name>, model or cap://skill...", use)
		}
	}
}

// toolPattern 把 tools/<source>/<name> 翻译为本地目录的选品模式
// (kind/provider 由供给源决定,用通配)。
func toolPattern(ref string) (string, error) {
	rest := strings.TrimPrefix(ref, "tools/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("bad tool reference %q: want tools/<source>/<name>", ref)
	}
	return fmt.Sprintf("cap://*.*/%s/%s", parts[0], parts[1]), nil
}

// crossNamespaceSkill 落实跨命名空间边界:cap:// 全名引用只允许
// kind=skill——skill 是命名空间的唯一公开接口。
func crossNamespaceSkill(refStr string, global *source.Catalog) (capability.Capability, error) {
	ref, err := capability.ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	if ref.Kind != "skill" {
		return nil, fmt.Errorf("%s: only cap://skill refs may cross namespaces (工具与 component 不出命名空间)", refStr)
	}
	return global.Get(refStr)
}

// modelStepCap 把默认模型包装为 use: model 步骤的能力:
// 渲染后的 args 即提示词,单次调用;步骤声明 context: fork 时,
// 以调用方对话快照 + 提示词起步。
func modelStepCap(m model.ToolCallingChatModel) capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "model", Provider: "step", Namespace: "internal", Name: "model"},
		Description: "单次模型调用",
	}, func(ctx context.Context, args string) (string, error) {
		out, err := m.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(args)))
		if err != nil {
			return "", err
		}
		return out.Content, nil
	})
}
