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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/skill"
	"github.com/joewm9911/agent-kit/source"
)

// ComponentConfig 声明一个执行单元:能力声明与能力使用分离的"声明"侧。
// 结构与 skill 声明同源(引擎+提示词+工具子集+可选专属模型),但不进
// 全局目录、没有对外参数——它的入参契约就是编排步骤传入的 args。
type ComponentConfig struct {
	Name   string       `yaml:"name"`
	Engine string       `yaml:"engine"` // react(默认)| plan-execute | 已注册模板
	Prompt prompt.Value `yaml:"prompt"`
	// Tools 是工具面引用:tools/<source>/<name|*>(本 ns 工具)、
	// components/<name>(本 ns 执行单元)、cap://skill 引用(跨 ns)。
	Tools        []string              `yaml:"tools"`
	Model        *ModelConfig          `yaml:"model"`
	MaxSteps     int                   `yaml:"max_steps"`
	EngineConfig map[string]any        `yaml:"engine_config"`
	Compaction   loop.CompactionConfig `yaml:"compaction"`
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
	global       *source.Catalog // 全局目录:skills 的去向,跨 ns skill 引用的来源
	prompts      *prompt.Resolver
	defaultModel model.ToolCallingChatModel
	maxRisk      capability.Risk
	loopPrompt   string
	toolTimeout  loop.Duration
	retry        loop.RetryConfig
	logger       *slog.Logger
}

// buildNamespace 装配一个命名空间:tools → components → skills,
// 只有 skills 进全局目录。
func buildNamespace(ctx context.Context, ns *NamespaceConfig, deps nsDeps) error {
	if ns.Name == "" {
		return fmt.Errorf("namespace: name is required")
	}

	// 1. 工具:进 ns 本地目录,对外不可见
	local := source.NewCatalog(deps.maxRisk, deps.logger)
	for _, tc := range ns.Tools {
		src, err := source.New(ctx, tc.Type, tc.Name, tc.Config)
		if err != nil {
			return fmt.Errorf("namespace %s: tool source %s: %w", ns.Name, tc.Name, err)
		}
		if err := local.AddSource(ctx, src, tc.Required, tc.Priority); err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
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
		if cc.Model != nil {
			decl.Model = &skill.ModelDecl{Provider: cc.Model.Provider, Config: cc.Model.Config}
		}
		c, err := skill.Build(ctx, decl, skill.Deps{
			Prompts:      deps.prompts,
			DefaultModel: deps.defaultModel,
			LoopPrompt:   deps.loopPrompt,
			Capabilities: caps,
			ToolTimeout:  deps.toolTimeout.Std(),
			Retry:        deps.retry,
		})
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		comps[cc.Name] = c
	}

	// 3. skills:编排引用 → 编译为 DAG → 进全局目录
	for i := range ns.Skills {
		sc := &ns.Skills[i]
		resolver := stepResolver(ns.Name, local, comps, deps.global, deps.defaultModel)
		c, err := skill.BuildGraph(ctx, &skill.GraphDeclaration{
			Name: sc.Name, Version: sc.Version, Description: sc.Description,
			Params: sc.Params, Steps: sc.Steps, Output: sc.Output,
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
// 渲染后的 args 即提示词,单次调用。
func modelStepCap(m model.ToolCallingChatModel) capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "model", Provider: "step", Namespace: "internal", Name: "model"},
		Description: "单次模型调用",
	}, func(ctx context.Context, args string) (string, error) {
		out, err := m.Generate(ctx, []*schema.Message{schema.UserMessage(args)})
		if err != nil {
			return "", err
		}
		return out.Content, nil
	})
}
