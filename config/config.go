// Package config 提供整个应用的声明式定义与总装:凭证、提示词源、能力源、
// skill、agent、serving、IM 通道,一份 YAML 全部描述,Build 组装为可运行
// 的 App。代码组织:schema.go 收口配置模型;config.go 单文件 Load/Build;
// agent.go agent 装配;app.go 多文件 LoadApp/BuildApp;namespace.go ns 装配。
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"

	einomodel "github.com/cloudwego/eino/components/model"
	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/resource"
	"github.com/joewm9911/agent-kit/protocol/secrets"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/serving"
	"github.com/joewm9911/agent-kit/skill"
)

// 四大上下文/记忆模块各自独立配置,各自的 store 槽用 cap://store/<kind>/
// <name> 引用 stores: 里的具名实例(或裸 type 作缺省简写)。四模块对称:
//   session → 会话短期记忆   memory → 长期记忆
//   todo    → 计划清单        digest → 大结果消化/暂存

// Load 读取单文件配置。与 LoadApp 同源经 resource 解析(file/embed/...):
// os 只出现在 file scheme 解析器与可写状态(state_dir)里,配置读取一律
// 走资源 FS。先解析 secrets 段构建凭证 provider(该段本身不得含占位符),
// 再展开 ${ENV} 与 ${secret:NAME},最后解析全文。
func Load(ref string) (*Config, error) {
	root, entry, err := resource.Resolve(ref)
	if err != nil {
		return nil, err
	}
	raw, err := fs.ReadFile(root, entry)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", entry, err)
	}
	sp, err := secretsProviderFor(raw, entry)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := expandParse(raw, sp, entry, &cfg); err != nil {
		return nil, err
	}
	cfg.root = root
	return &cfg, nil
}

// secretsProviderFor 从配置头部的 secrets 段构建凭证 provider
// (该段本身不得含占位符)。
func secretsProviderFor(raw []byte, path string) (secrets.Provider, error) {
	var head struct {
		Secrets SecretsConfig `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// 经注册表构造(env 随协议包常驻;file/vault 等在 impl/secrets/*,
	// 空导入注册,未注册即 fail fast)。
	return secrets.New(head.Secrets.Provider, head.Secrets.Config)
}

// expandParse 用给定 provider 展开占位符后解析 YAML。严格模式:未知
// 字段直接报错——拼错的键(max_round、system_prompt)静默忽略等于配置
// 没生效还不吱声,是全库审计里最大的一类静默吞配置。自由形态段
// (config/engine_config/args 等)声明为 map 不受影响。
func expandParse(raw []byte, sp secrets.Provider, path string, out any) error {
	expanded, err := secrets.Expand(raw, sp)
	if err != nil {
		return fmt.Errorf("expand secrets in %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) { // 空文件按零值处理,与 Unmarshal 一致
			return nil
		}
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// App 是总装产物。
type App struct {
	Agents  map[string]*agent.Agent
	Catalog *source.Catalog
	Prompts *prompt.Resolver
	Server  *serving.Server // serving 未配置时为 nil
	// AgentMounts 是多文件路径下各 agent 的挂载目录(关联 namespaces
	// 导出的 skills),供巡检与调试;单文件路径为 nil。
	AgentMounts map[string]*source.Catalog
}

// BuildOptions 是代码侧的注入点。
type BuildOptions struct {
	// Interactor 是 agent 的默认交互通道(CLI 场景传 interact.NewCLI());
	// IM 通道会在各自会话里覆盖它。
	Interactor runctx.Interactor
	// ExtraCapabilities 是代码侧构造的能力(local.Func、rpctool、子 agent),
	// 以 "local" source 入目录。
	ExtraCapabilities []capability.Capability
	Logger            *slog.Logger
}

// Build 把声明组装为可运行的 App。装配顺序:观测 → 提示词 → 目录
// (sources → skills)→ agents → gateway/channels。
func Build(ctx context.Context, cfg *Config, opts BuildOptions) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 1. 可观测性(进程级幂等账本在 observe.go)
	if err := installObservability(cfg.Observability, logger); err != nil {
		return nil, err
	}

	if err := cfg.Profile.rejectLegacyKeys("app"); err != nil {
		return nil, err
	}
	if err := rejectWorkDir(cfg.WorkDirLegacy, "app"); err != nil {
		return nil, err
	}
	if cfg.DefaultModelLegacy != nil {
		return nil, fmt.Errorf("app: default_model has been renamed model (part of the execution profile; same provider/config shape)")
	}

	// 2. 提示词
	var prompts *prompt.Resolver
	if len(cfg.Prompts.Sources) > 0 {
		prompts = prompt.NewResolver(cfg.Prompts.DefaultLabel)
		for _, ps := range cfg.Prompts.Sources {
			p, err := buildPromptProvider(ps, cfg.root)
			if err != nil {
				return nil, fmt.Errorf("prompt source %s: %w", ps.Name, err)
			}
			prompts.Add(ps.Name, p)
		}
	}

	// 3. 目录:准入 → sources → 代码侧能力
	maxRisk := capability.RiskMutating
	if cfg.Catalog.MaxRisk != "" {
		var err error
		if maxRisk, err = capability.ParseRisk(cfg.Catalog.MaxRisk); err != nil {
			return nil, err
		}
	}
	catalog := source.NewCatalog(maxRisk, logger)
	for _, sc := range cfg.Sources {
		sconf := sc.Config
		if sc.Type == "exec" {
			sconf = cfg.Exec.injectInto(sconf)
		}
		src, err := source.New(ctx, sc.Type, sc.Name, sconf)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", sc.Name, err)
		}
		if err := catalog.AddSource(ctx, src, sc.Required, sc.Priority); err != nil {
			return nil, err
		}
	}
	if len(opts.ExtraCapabilities) > 0 {
		if err := catalog.Add(opts.ExtraCapabilities...); err != nil {
			return nil, err
		}
	}

	// app 层默认模型(skill/workflow 与未声明 model 的 agent 使用)。
	// 统一套 Ring 0 中间件:瞬时错误重试 + 预算(门闸经 ctx 生效,
	// skill/component 内部调用同样计入调用方会话预算)。
	var defaultModel einomodel.ToolCallingChatModel
	if cfg.Profile.Model != nil {
		var err error
		if defaultModel, err = model.Build(ctx, cfg.Profile.Model.Provider, cfg.Profile.Model.Config); err != nil {
			return nil, fmt.Errorf("model: %w", err)
		}
		defaultModel = loop.BudgetModel(loop.RetryModel(defaultModel, cfg.Profile.retry())) // 质量守卫在循环装配层(ReviewModel)
	}

	// 具名解析环境:agent 注册表(agents 建成后回填)+ 具名模型 Hub。
	agentNames := make([]string, 0, len(cfg.Agents))
	for i := range cfg.Agents {
		agentNames = append(agentNames, cfg.Agents[i].Name)
	}
	hubs, err := newSkillHubs(cfg.Models, cfg.Profile.retry(), agentNames)
	if err != nil {
		return nil, err
	}

	// 4. skills:装配后入目录,供 agent 选品。条目二选一:内部声明走
	// skill.Build(确定性编排),use: 外部链接走 skillpack 全链路
	// (物化 → 校验 → 隔离子循环,治理与内部同源)。
	packOpts, err := cfg.Skillpacks.options()
	if err != nil {
		return nil, err
	}
	packRoot := cfg.Skillpacks.root(cfg.StateDir)
	for _, entry := range cfg.Skills {
		skillDeps := skill.Deps{
			Todo:    componentTodo(),
			Catalog: catalog, Prompts: prompts, DefaultModel: defaultModel,
			ToolTimeout: cfg.Profile.toolTimeout().Std(), Retry: cfg.Profile.retry(),
		}
		var c capability.Capability
		if entry.Use != "" {
			return nil, fmt.Errorf(`skill %s: use has been renamed from (the external fetch source): from: %q`, entry.Name, entry.Use)
		}
		if entry.From != "" {
			if !isExternalRef(entry.From) {
				return nil, fmt.Errorf("skill %s: from on flat skills only supports external links (github.com/...|https://...|file:...); for internal delegation use namespaces", entry.From)
			}
			if !entry.Prompt.IsZero() || entry.Engine != "" || len(entry.Capabilities.Include) > 0 {
				return nil, fmt.Errorf("skill %s: from (external ref) is mutually exclusive with prompt/engine/capabilities", entry.From)
			}
			c, err = buildSkillpack(ctx, packRoot, packOpts,
				skill.PackSpec{Use: entry.From, Integrity: entry.Integrity, Name: entry.Name},
				skill.PackOverrides{Model: entry.Model, MaxSteps: entry.MaxSteps,
					Tools: entry.Tools, Context: entry.Context},
				skillDeps, cfg.Exec, hubs)
		} else {
			c, err = skill.Build(ctx, &entry.Declaration, skillDeps)
		}
		if err != nil {
			return nil, err
		}
		if err := catalog.Add(c); err != nil {
			return nil, err
		}
	}

	// 5b. namespaces:三层结构装配(tools → components → skills),
	// 只有 skills 进全局目录;声明顺序决定跨 ns 引用的可见性。
	nsExports := newComponentExports() // 导出 component 注册表(本装配序列共享)
	for i := range cfg.Namespaces {
		err := buildNamespace(ctx, &cfg.Namespaces[i], nsDeps{
			exports: nsExports,
			global:  catalog, prompts: prompts, defaultModel: defaultModel,
			maxRisk: maxRisk, base: cfg.Profile, appModel: cfg.Profile.Model, logger: logger,
			packRoot: packRoot, packOpts: packOpts, execCfg: cfg.Exec, hubs: hubs,
		})
		if err != nil {
			return nil, err
		}
	}

	// 6. agents
	app := &App{Agents: map[string]*agent.Agent{}, Catalog: catalog, Prompts: prompts}
	for i := range cfg.Agents {
		ac := &cfg.Agents[i]
		var caps []capability.Capability
		if len(ac.Capabilities.Include) > 0 {
			var err error
			if caps, err = catalog.Select(ac.Capabilities.Include, ac.Capabilities.Exclude); err != nil {
				return nil, fmt.Errorf("agent %s: %w", ac.Name, err)
			}
		}
		eff := cfg.Profile.merge(ac.Profile)
		m, err := agentModel(ctx, ac.Profile.Model, defaultModel, eff.retry())
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", ac.Name, err)
		}
		a, err := buildAgent(ctx, ac, eff, caps, prompts, m, opts.Interactor, logger)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", ac.Name, err)
		}
		app.Agents[a.Name()] = a
		hubs.agents.add(a.Name(), a) // frontmatter agent: 的调用期解析
	}

	// 7. gateway 与 IM 通道
	if cfg.Serving.Addr != "" {
		agents := make([]serving.Runnable, 0, len(app.Agents))
		for _, a := range app.Agents {
			agents = append(agents, a)
		}
		app.Server = serving.New(cfg.Serving.Addr, agents, logger)
		dispatcher := serving.NewDispatcher(logger)
		// 单文件形态没有顶层具名 stores,suspend.store 只支持裸 type。
		if kv, err := suspendKV(cfg.Suspend, nil); err != nil {
			return nil, err
		} else if kv != nil {
			dispatcher.EnableSuspend(kv)
			app.Server.EnableSuspend(kv) // HTTP /messages 与 IM 共用同一挂起后端
		}
		for _, cc := range cfg.Channels {
			ch, err := channel.New(cc.Type, cc.Name, cc.Config)
			if err != nil {
				return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
			}
			target, ok := app.Agents[cc.Agent]
			if !ok {
				return nil, fmt.Errorf("channel %s: unknown agent %q", cc.Name, cc.Agent)
			}
			binding := serving.Binding{
				Channel: ch, Agent: target,
				SessionMapping: cc.SessionMapping, ReplyMode: cc.ReplyMode,
				Placeholder: cc.Placeholder,
			}
			// 面向用户文案覆盖(未知键 fail fast)。
			texts, err := serving.NewTexts(cc.Texts)
			if err != nil {
				return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
			}
			binding.Texts = texts
			// 按名解析装饰器/进度订阅(代码注册、配置启用,查无 fail fast)。
			if cc.Decorator != "" {
				dec, err := serving.LookupDecorator(cc.Decorator)
				if err != nil {
					return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
				}
				binding.Decorator = dec
			}
			if cc.OnProgress != "" {
				ph, err := serving.LookupProgressHandler(cc.OnProgress)
				if err != nil {
					return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
				}
				binding.OnProgress = ph
			}
			err = app.Server.AttachChannel(ctx, ch, dispatcher, binding)
			if err != nil {
				return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
			}
		}
	} else if len(cfg.Channels) > 0 {
		return nil, fmt.Errorf("channels configured but serving.addr is empty")
	}

	return app, nil
}
