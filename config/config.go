// Package config 提供整个应用的声明式定义与总装:凭证、提示词源、能力源、
// skill、agent、serving、IM 通道,一份 YAML 全部描述,Build 组装为可运行
// 的 App。代码组织:schema.go 收口配置模型;config.go 单文件 Load/Build;
// agent.go agent 装配;app.go 多文件 LoadApp/BuildApp;namespace.go ns 装配。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	einomodel "github.com/cloudwego/eino/components/model"
	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
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

// Load 读取配置文件:先解析 secrets 段构建凭证 provider(该段本身
// 不得含占位符),再展开 ${ENV} 与 ${secret:NAME},最后解析全文。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sp, err := secretsProviderFor(raw, path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := expandParse(raw, sp, path, &cfg); err != nil {
		return nil, err
	}
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

// expandParse 用给定 provider 展开占位符后解析 YAML。
func expandParse(raw []byte, sp secrets.Provider, path string, out any) error {
	expanded, err := secrets.Expand(raw, sp)
	if err != nil {
		return fmt.Errorf("expand secrets in %s: %w", path, err)
	}
	if err := yaml.Unmarshal(expanded, out); err != nil {
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
// (sources → skills → workflows)→ agents → gateway/channels。
func Build(ctx context.Context, cfg *Config, opts BuildOptions) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 1. 可观测性(进程级幂等账本在 observe.go)
	if err := installObservability(cfg.Observability, logger); err != nil {
		return nil, err
	}

	// 2. 提示词
	var prompts *prompt.Resolver
	if len(cfg.Prompts.Sources) > 0 {
		prompts = prompt.NewResolver(cfg.Prompts.DefaultLabel)
		for _, ps := range cfg.Prompts.Sources {
			p, err := prompt.NewProvider(ps.Type, ps.Config)
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
		defaultModel = loop.FinishGuard(loop.BudgetModel(loop.RetryModel(defaultModel, cfg.Profile.retry())))
	}

	// 4. skills:装配后入目录,供 agent 选品。条目二选一:内部声明走
	// skill.Build(确定性编排),use: 外部链接走 skillpack 全链路
	// (物化 → 校验 → 隔离子循环,治理与内部同源)。
	packOpts, err := cfg.Skillpacks.options()
	if err != nil {
		return nil, err
	}
	packRoot := cfg.Skillpacks.root(cfg.WorkDir)
	for _, entry := range cfg.Skills {
		skillDeps := skill.Deps{
			Todo:    componentTodo(),
			Catalog: catalog, Prompts: prompts, DefaultModel: defaultModel,
			ToolTimeout: cfg.Profile.toolTimeout().Std(), Retry: cfg.Profile.retry(),
		}
		var c capability.Capability
		if entry.Use != "" {
			if !isExternalRef(entry.Use) {
				return nil, fmt.Errorf("skill %s: 平铺 skills 的 use 只支持外部链接(github.com/...|https://...|file:...),内部委托请用 namespaces", entry.Use)
			}
			if !entry.Prompt.IsZero() || entry.Engine != "" || len(entry.Capabilities.Include) > 0 {
				return nil, fmt.Errorf("skill %s: use(外部引用)与 prompt/engine/capabilities 互斥", entry.Use)
			}
			c, err = buildSkillpack(ctx, packRoot, packOpts,
				skill.PackSpec{Use: entry.Use, Integrity: entry.Integrity, Name: entry.Name},
				skill.PackOverrides{Model: entry.Model, MaxSteps: entry.MaxSteps,
					Tools: entry.Tools, Context: entry.Context},
				skillDeps, cfg.Exec)
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
	for i := range cfg.Namespaces {
		err := buildNamespace(ctx, &cfg.Namespaces[i], nsDeps{
			global: catalog, prompts: prompts, defaultModel: defaultModel,
			maxRisk: maxRisk, base: cfg.Profile, appModel: cfg.Profile.Model, logger: logger,
			packRoot: packRoot, packOpts: packOpts, execCfg: cfg.Exec,
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
			err = app.Server.AttachChannel(ctx, ch, dispatcher, serving.Binding{
				Channel: ch, Agent: target,
				SessionMapping: cc.SessionMapping, ReplyMode: cc.ReplyMode,
			})
			if err != nil {
				return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
			}
		}
	} else if len(cfg.Channels) > 0 {
		return nil, fmt.Errorf("channels configured but serving.addr is empty")
	}

	return app, nil
}
