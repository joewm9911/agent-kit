// namespace.go 实现命名空间(能力清单文件)的装配:
//
//	namespace
//	├── sources      工具供给源(mcp/http/...),ns 内共享,对外不可见
//	├── skills       过程卡(内联 prompt+params)或外部 SKILL.md 包,进目录
//	└── subagents    声明式 sub-agent(同构隔离子循环),进目录
//
// 边界规则在装配期落实:工具引用不出命名空间(内联卡的直挂是唯一
// 显式豁免——主循环亲自执行是该形态的定义);skills/subagents 挂载
// 即对 agent 可见,跨文件同名在目录冲突检测处报错。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/skill"

	"github.com/cloudwego/eino/components/model"
)

type nsDeps struct {
	global       *source.Catalog // skills/subagents 的落点(agent 的挂载目录)
	packRoot     string          // 外部 skillpack 的物化目录(.skills)
	packOpts     skill.PackOptions
	execCfg      ExecConfig // app 级默认沙箱策略(透传给 ns 内 exec 源与 pack 的 exec 工具)
	hubs         *skillHubs // frontmatter agent:/model: 的按名解析环境
	prompts      *prompt.Resolver
	defaultModel model.ToolCallingChatModel
	maxRisk      capability.Risk
	loopPrompt   string
	// base 是本 ns 之上各层合并好的执行画像(app.merge(agent自己));
	// buildNamespace 内再叠加 ns 自己的 Profile,subagent 再叠加自己的,
	// 最后叠加 mount 覆盖——五级就近合并。
	base Profile
	// mount 是 agent 给本 namespace 的 per-mount 覆盖画像(最高优;
	// 单文件路径为空 Profile)。
	mount Profile
	// appModel 是 app 层 model(判断 subagent 解析出的 model 是否为
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

// buildNamespace 装配一个命名空间:sources → skills → subagents,
// skills/subagents 进 deps.global(agent 的挂载目录)。
func buildNamespace(ctx context.Context, ns *NamespaceConfig, deps nsDeps) error {
	if ns.Name == "" {
		return fmt.Errorf("namespace: name is required")
	}
	if len(ns.ImportsLegacy) > 0 {
		return fmt.Errorf("namespace %s: imports has been removed — visibility is mount order: skills and subagents are visible to an agent once their namespace is mounted (cross-namespace component wiring is gone with the orchestration family)", ns.Name)
	}
	if len(ns.ComponentsLegacy) > 0 {
		return fmt.Errorf("namespace %s: components has been removed — a prompt+tools declaration is a skill (skills:, procedure card the host loop executes); an isolated executor is a sub-agent (subagents:, always the standard loop); fixed flows live in host code via eino compose (see examples/pipeline)", ns.Name)
	}
	// 能力不可自指 model:namespace 的执行画像不得声明 model。
	if err := ns.Profile.validateNoModel("namespace " + ns.Name); err != nil {
		return err
	}
	if err := ns.Profile.rejectLegacyKeys("namespace " + ns.Name); err != nil {
		return err
	}
	if err := deps.mount.rejectLegacyKeys("namespace " + ns.Name + " (mount override)"); err != nil {
		return err
	}
	if len(ns.ToolsLegacy) > 0 {
		return fmt.Errorf("namespace %s: tools has been renamed sources (it declares capability sources, isomorphic to the top-level sources:; the tools that references the tool surface keeps its meaning)", ns.Name)
	}

	// 1. 工具:进 ns 本地目录,对外不可见。Sync 结果按 (ns 文件, 源名)
	// 缓存——同一 namespace 被多个 agent 实例化时源连接只建一次。
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

	// 执行画像链:base = app.merge(agent),叠加 ns 自己 → nsBase;
	// nsEff = nsBase.merge(mount) 供 ns 级(from 包)使用;每个 subagent
	// 再叠加自己的 Profile 后叠加 mount(mount 最高优)。
	nsBase := deps.base.merge(ns.Profile)
	nsEff := nsBase.merge(deps.mount)

	// 2. skills:内联卡(工具直挂宿主 + 卡片进目录)或 from 外部包。
	for i := range ns.Skills {
		sc := &ns.Skills[i]
		if err := rejectRemovedNamespaceSkillKeys(ns.Name, sc); err != nil {
			return err
		}
		if sc.From != "" {
			if !isExternalRef(sc.From) {
				return fmt.Errorf("namespace %s: skill %s: from only accepts external links (github.com/...|https://...|file:...)", ns.Name, sc.Name)
			}
			if !sc.Prompt.IsZero() {
				return fmt.Errorf("namespace %s: skill %s: from and prompt are mutually exclusive (an external pack brings its own SKILL.md body)", ns.Name, sc.Name)
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
		// 内联卡:SKILL.md 的配置层等价物。
		if sc.MaxRounds != 0 {
			return fmt.Errorf("namespace %s: skill %s: max_rounds only applies to from (external skillpack) skills — a card has no inner loop; an isolated executor with a round budget is a sub-agent (subagents:)", ns.Name, sc.Name)
		}
		if sc.Context != "" {
			return fmt.Errorf("namespace %s: skill %s: context only applies to from (external skillpack) skills — a card shares the host context by definition; an isolated executor declares context on subagents:", ns.Name, sc.Name)
		}
		caps, err := resolveToolFace(ns.Name, sc.Tools, local)
		if err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
		// 卡片声明的工具直挂宿主目录(主循环亲自执行是该形态的定义,
		// 此为"工具不出命名空间"的唯一显式豁免);同工具被多张卡引用
		// 时幂等跳过。
		for _, tc := range caps {
			if _, err := deps.global.Get(tc.Meta().Ref.String()); err == nil {
				continue // 已挂载(多卡共用/多 ns 同源)
			}
			if err := deps.global.Add(tc); err != nil {
				return fmt.Errorf("namespace %s: skill %s: mount tool %s: %w", ns.Name, sc.Name, tc.Meta().Ref, err)
			}
		}
		c, err := skill.Build(ctx, &skill.Declaration{
			Name: ns.Name + "/" + sc.Name, Version: sc.Version,
			Description: sc.Description, Params: sc.Params, Prompt: sc.Prompt,
		}, skill.Deps{Prompts: deps.prompts})
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
		if err := deps.global.Add(c); err != nil {
			return fmt.Errorf("namespace %s: skill %s: %w", ns.Name, sc.Name, err)
		}
	}

	// 3. subagents:同构隔离子循环,进目录(cap://agent/<ns>/<name>)。
	for i := range ns.Subagents {
		ac := &ns.Subagents[i]
		if ac.Name == "" {
			return fmt.Errorf("namespace %s: subagent name is required", ns.Name)
		}
		// 能力不可自指 model:sub-agent 的执行画像不得声明 model。
		if err := ac.Profile.validateNoModel(fmt.Sprintf("namespace %s: subagent %s", ns.Name, ac.Name)); err != nil {
			return err
		}
		if err := ac.Profile.rejectLegacyKeys(fmt.Sprintf("namespace %s: subagent %s", ns.Name, ac.Name)); err != nil {
			return err
		}
		if len(ac.StepsLegacy) > 0 || ac.OutputLegacy != nil {
			return fmt.Errorf("namespace %s: subagent %s: steps/output has been removed along with the orchestration family — fixed flows live in host code via eino compose (see examples/pipeline); a sub-agent is a prompt+tools loop", ns.Name, ac.Name)
		}
		if ac.ExportLegacy != nil {
			return fmt.Errorf("namespace %s: subagent %s: export has been removed — mounted namespaces expose their skills and subagents to the mounting agent directly", ns.Name, ac.Name)
		}
		// sub-agent 生效画像:nsBase.merge(自己) 再叠加 mount(最高优)。
		// 五级就近合并:mount > subagent > namespace > agent > app。
		eff := nsBase.merge(ac.Profile).merge(deps.mount)
		caps, err := resolveToolFace(ns.Name, ac.Tools, local)
		if err != nil {
			return fmt.Errorf("namespace %s: subagent %s: %w", ns.Name, ac.Name, err)
		}
		decl := &skill.AgentDecl{
			Name: ns.Name + "/" + ac.Name, Version: ac.Version,
			Description: ac.Description, Params: ac.Params, Prompt: ac.Prompt,
			Context: ac.Context, Deliver: ac.Deliver, Todo: ac.Todo,
			MaxSteps:           eff.maxSteps(),
			Compaction:         eff.compaction(),
			EngineLegacy:       ac.EngineLegacy,
			EngineConfigLegacy: ac.EngineConfigLegacy,
			ModeLegacy:         ac.ModeLegacy,
		}
		// model 走执行画像三级链(mount > agent > app;ns/subagent 不可自指)。
		// 解析出的 model 与 app 默认相同 → 复用共享 DefaultModel(不重建);
		// 不同(agent/mount 指定)→ 为其构建专属模型。
		if eff.Model != nil && eff.Model != deps.appModel {
			decl.Model = &skill.ModelDecl{Provider: eff.Model.Provider, Config: eff.Model.Config}
		} else if ac.Profile.Reliability.Retry != nil && deps.logger != nil {
			// 重试是模型中间件:复用共享模型时,该模型携带的是 app 层
			// 重试,subagent 声明的 retry 装不上——静默无效不如说清楚。
			deps.logger.Warn("subagent reliability.retry only takes effect with a dedicated model (mount/agent-specified); the shared default model carries the app-level retry",
				"namespace", ns.Name, "subagent", ac.Name)
		}
		c, err := skill.BuildAgent(ctx, decl, skill.Deps{
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
		if err := deps.global.Add(c); err != nil {
			return fmt.Errorf("namespace %s: subagent %s: %w", ns.Name, ac.Name, err)
		}
	}
	return nil
}

// rejectRemovedNamespaceSkillKeys 落实编排族硬切:steps/use/engine/output/
// deliver/step_defaults 一律装配期报错、文案自带迁移路径。
func rejectRemovedNamespaceSkillKeys(nsName string, sc *NamespaceSkill) error {
	switch {
	case len(sc.StepsLegacy) > 0:
		return fmt.Errorf("namespace %s: skill %s: steps has been removed — fixed flows live in host code via eino compose + AsLambda (see examples/pipeline); a declarative skill is a prompt+params card the host loop executes, or a from: SKILL.md pack", nsName, sc.Name)
	case sc.UseLegacy != nil:
		return fmt.Errorf("namespace %s: skill %s: use has been removed — declare the target directly: a card (prompt+params), a from: pack, or a sub-agent under subagents:", nsName, sc.Name)
	case sc.EngineLegacy != nil:
		return fmt.Errorf("namespace %s: skill %s: engine has been removed — skills run on the host loop; an isolated executor is a sub-agent (subagents:), always the standard loop", nsName, sc.Name)
	case sc.OutputLegacy != nil:
		return fmt.Errorf("namespace %s: skill %s: output has been removed along with the orchestration family", nsName, sc.Name)
	case sc.DeliverLegacy != nil:
		return fmt.Errorf("namespace %s: skill %s: deliver on a skill has been removed (a card's output is authored by the host model) — declare a sub-agent with deliver: under subagents: for mechanical fidelity", nsName, sc.Name)
	case sc.StepDefaultsLegacy != nil:
		return fmt.Errorf("namespace %s: skill %s: step_defaults has been removed along with the orchestration family", nsName, sc.Name)
	}
	return nil
}

// resolveToolFace 解析 skills/subagents 的工具面引用:tools/<source>/<name|*>
// (本 ns 工具,允许通配批量展开)。工具不出命名空间。
func resolveToolFace(nsName string, refs []string, local *source.Catalog) ([]capability.Capability, error) {
	var out []capability.Capability
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "tools/") {
			return nil, fmt.Errorf("bad reference %q: want tools/<source>/<name|*> (components/ and cap:// step references are gone with the orchestration family)", ref)
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
			return nil, fmt.Errorf("%s matches no tool in namespace %s (tools do not cross namespaces)", ref, nsName)
		}
		out = append(out, caps...)
	}
	return out, nil
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
