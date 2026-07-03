// Package config 提供整个应用的声明式定义与总装:
// 凭证、提示词源、能力源、skill、workflow、agent、serving、IM 通道,
// 一份 YAML 全部描述,Build 组装为可运行的 App。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"gopkg.in/yaml.v3"

	"github.com/cloverzhang/agent-kit/agent"
	"github.com/cloverzhang/agent-kit/builtin"
	"github.com/cloverzhang/agent-kit/capability"
	"github.com/cloverzhang/agent-kit/channel"
	"github.com/cloverzhang/agent-kit/engine"
	"github.com/cloverzhang/agent-kit/loop"
	"github.com/cloverzhang/agent-kit/memory"
	"github.com/cloverzhang/agent-kit/observe"
	"github.com/cloverzhang/agent-kit/prompt"
	"github.com/cloverzhang/agent-kit/registry"
	"github.com/cloverzhang/agent-kit/runctx"
	"github.com/cloverzhang/agent-kit/secrets"
	"github.com/cloverzhang/agent-kit/serving"
	"github.com/cloverzhang/agent-kit/session"
	"github.com/cloverzhang/agent-kit/skill"
	"github.com/cloverzhang/agent-kit/source"
	"github.com/cloverzhang/agent-kit/workflow"
)

// ModelConfig 声明一个模型。
type ModelConfig struct {
	Provider string         `yaml:"provider"`
	Config   map[string]any `yaml:"config"`
}

// SourceConfig 声明一个能力供给源。
type SourceConfig struct {
	Name     string         `yaml:"name"`
	Type     string         `yaml:"type"`
	Required bool           `yaml:"required"`
	Priority int            `yaml:"priority"`
	Config   map[string]any `yaml:"config"`
}

// PromptSourceConfig 声明一个提示词供给源。
type PromptSourceConfig struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Config map[string]any `yaml:"config"`
}

// AgentConfig 声明一个 agent:唯一的主循环是 ReAct,没有 pattern 字段;
// plan-execute 等结构以 skill 的形式出现在 capabilities 里。
type AgentConfig struct {
	Name         string       `yaml:"name"`
	Description  string       `yaml:"description"`
	Model        *ModelConfig `yaml:"model"` // nil 用顶层 default_model
	SystemPrompt prompt.Value `yaml:"system_prompt"` // L2 业务 persona
	LoopPrompt   prompt.Value `yaml:"loop_prompt"`   // L1 框架规约覆盖(默认内置)
	MaxSteps     int          `yaml:"max_steps"`

	Capabilities struct {
		Include []string `yaml:"include"`
		Exclude []string `yaml:"exclude"`
	} `yaml:"capabilities"`

	// 内置能力开关(默认开启,显式 false 关闭)。
	Todo    *bool `yaml:"todo"`
	AskUser *bool `yaml:"ask_user"`

	Memory struct {
		Window      int            `yaml:"window"`       // 0 = 不启用会话记忆
		Store       string         `yaml:"store"`        // inmemory(默认)| file | session.Register 注册的自定义类型
		StoreConfig map[string]any `yaml:"store_config"` //
		// 长期记忆:挂载 memory_save/search 工具,后端可换
		// (inmemory 默认 | memory.Register 注册的自定义类型)。
		LongTermTools  bool           `yaml:"long_term_tools"`
		LongTermStore  string         `yaml:"long_term_store"`
		LongTermConfig map[string]any `yaml:"long_term_config"`
		// AutoRecall 启用 L4 自动召回:每轮用当前输入检索长期记忆与
		// 窗口外的会话历史,命中片段注入 system prompt(标注"非指令")。
		AutoRecall struct {
			TopK int `yaml:"top_k"` // 0 = 不启用
		} `yaml:"auto_recall"`
	} `yaml:"memory"`

	// MaxToolResultLen 是工具结果进入上下文的截断长度(rune):
	// 0 = 默认 8000,-1 = 关闭截断。防 MCP 等外部工具返回超大结果打爆窗口。
	MaxToolResultLen int `yaml:"max_tool_result_len"`

	Budget           loop.BudgetConfig     `yaml:"budget"`
	Compaction       loop.CompactionConfig `yaml:"compaction"`
	StructuredOutput loop.StructuredConfig `yaml:"structured_output"`
	// Approval:interactive(默认)| auto | deny。
	Approval string `yaml:"approval"`
}

// ChannelConfig 声明一个 IM 通道绑定。
type ChannelConfig struct {
	Name           string         `yaml:"name"`
	Type           string         `yaml:"type"`
	Agent          string         `yaml:"agent"`
	SessionMapping string         `yaml:"session_mapping"` // chat | chat_user
	ReplyMode      string         `yaml:"reply_mode"`      // text | stream
	Config         map[string]any `yaml:"config"`
}

// Config 是应用的完整声明。
type Config struct {
	Secrets struct {
		Provider string         `yaml:"provider"` // env(默认)| file
		Config   map[string]any `yaml:"config"`
	} `yaml:"secrets"`

	PromptSources      []PromptSourceConfig `yaml:"prompt_sources"`
	PromptDefaultLabel string               `yaml:"prompt_default_label"`

	Sources []SourceConfig `yaml:"sources"`
	Catalog struct {
		MaxRisk string `yaml:"max_risk"` // 准入上限,默认 mutating(dangerous 不入目录)
	} `yaml:"catalog"`

	DefaultModel *ModelConfig         `yaml:"default_model"`
	Skills       []*skill.Declaration `yaml:"skills"`
	Workflows    []workflow.Config    `yaml:"workflows"`
	Agents       []AgentConfig        `yaml:"agents"`

	Serving struct {
		Addr string `yaml:"addr"`
	} `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`

	Observability struct {
		Log            bool   `yaml:"log"`
		TrajectoryPath string `yaml:"trajectory_path"`
	} `yaml:"observability"`
}

// Load 读取配置文件:先解析 secrets 段构建凭证 provider(该段本身
// 不得含占位符),再展开 ${ENV} 与 ${secret:NAME},最后解析全文。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var head struct {
		Secrets struct {
			Provider string         `yaml:"provider"`
			Config   map[string]any `yaml:"config"`
		} `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var sp secrets.Provider
	switch head.Secrets.Provider {
	case "", "env":
		sp = secrets.Env{}
	case "file":
		p, _ := head.Secrets.Config["path"].(string)
		if sp, err = secrets.NewFile(p); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown secrets provider %q", head.Secrets.Provider)
	}

	expanded, err := secrets.Expand(raw, sp)
	if err != nil {
		return nil, fmt.Errorf("expand secrets in %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// App 是总装产物。
type App struct {
	Agents  map[string]*agent.Agent
	Catalog *source.Catalog
	Prompts *prompt.Resolver
	Server  *serving.Server // serving 未配置时为 nil
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

	// 1. 可观测性
	if cfg.Observability.Log {
		observe.Install(logger)
	}
	if p := cfg.Observability.TrajectoryPath; p != "" {
		h, err := observe.Trajectory(p)
		if err != nil {
			return nil, err
		}
		callbacks.AppendGlobalHandlers(h)
	}

	// 2. 提示词
	var prompts *prompt.Resolver
	if len(cfg.PromptSources) > 0 {
		prompts = prompt.NewResolver(cfg.PromptDefaultLabel)
		for _, ps := range cfg.PromptSources {
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
		src, err := source.New(ctx, sc.Type, sc.Name, sc.Config)
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

	// 默认模型(skill/workflow 与未声明 model 的 agent 使用)
	var defaultModel model.ToolCallingChatModel
	if cfg.DefaultModel != nil {
		var err error
		if defaultModel, err = registry.BuildModel(ctx, cfg.DefaultModel.Provider, cfg.DefaultModel.Config); err != nil {
			return nil, fmt.Errorf("default_model: %w", err)
		}
	}

	// 4. skills:装配后入目录,供 agent 选品
	for _, decl := range cfg.Skills {
		c, err := skill.Build(ctx, decl, skill.Deps{
			Catalog: catalog, Prompts: prompts, DefaultModel: defaultModel,
		})
		if err != nil {
			return nil, err
		}
		if err := catalog.Add(c); err != nil {
			return nil, err
		}
	}

	// 5. workflows:编译后入目录(可被 agent 挂载,也可经 A2A 暴露)
	for _, wf := range cfg.Workflows {
		c, err := workflow.Build(ctx, wf, catalog, defaultModel)
		if err != nil {
			return nil, err
		}
		if err := catalog.Add(c); err != nil {
			return nil, err
		}
	}

	// 6. agents
	app := &App{Agents: map[string]*agent.Agent{}, Catalog: catalog, Prompts: prompts}
	for i := range cfg.Agents {
		a, err := buildAgent(ctx, &cfg.Agents[i], catalog, prompts, defaultModel, opts.Interactor)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", cfg.Agents[i].Name, err)
		}
		app.Agents[a.Name()] = a
	}

	// 7. gateway 与 IM 通道
	if cfg.Serving.Addr != "" {
		agents := make([]*agent.Agent, 0, len(app.Agents))
		for _, a := range app.Agents {
			agents = append(agents, a)
		}
		app.Server = serving.New(cfg.Serving.Addr, agents, logger)
		dispatcher := channel.NewDispatcher(logger)
		for _, cc := range cfg.Channels {
			ch, err := channel.New(cc.Type, cc.Name, cc.Config)
			if err != nil {
				return nil, fmt.Errorf("channel %s: %w", cc.Name, err)
			}
			target, ok := app.Agents[cc.Agent]
			if !ok {
				return nil, fmt.Errorf("channel %s: unknown agent %q", cc.Name, cc.Agent)
			}
			err = app.Server.AttachChannel(ctx, ch, dispatcher, channel.Binding{
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

func buildAgent(ctx context.Context, ac *AgentConfig, catalog *source.Catalog,
	prompts *prompt.Resolver, defaultModel model.ToolCallingChatModel,
	interactor runctx.Interactor) (*agent.Agent, error) {

	// 模型:专属或默认,先套预算(Ring 0,模型没得选)
	m := defaultModel
	if ac.Model != nil {
		var err error
		if m, err = registry.BuildModel(ctx, ac.Model.Provider, ac.Model.Config); err != nil {
			return nil, err
		}
	}
	if m == nil {
		return nil, fmt.Errorf("no model (declare model or default_model)")
	}
	m, _ = loop.WrapModel(m, ac.Budget)

	// 能力选品 + 内置能力 + 审批闸门
	var caps []capability.Capability
	if len(ac.Capabilities.Include) > 0 {
		var err error
		if caps, err = catalog.Select(ac.Capabilities.Include, ac.Capabilities.Exclude); err != nil {
			return nil, err
		}
	}
	if ac.Todo == nil || *ac.Todo {
		caps = append(caps, builtin.TodoCapabilities()...)
	}
	if ac.AskUser == nil || *ac.AskUser {
		caps = append(caps, builtin.AskUser())
	}
	var kv memory.KV
	if ac.Memory.LongTermTools {
		var err error
		if kv, err = memory.New(ac.Memory.LongTermStore, ac.Memory.LongTermConfig); err != nil {
			return nil, err
		}
		caps = append(caps, memory.AsCapabilities(kv)...)
	}
	mode := loop.ApprovalMode(ac.Approval)
	if mode == "" {
		mode = loop.ApprovalInteractive
	}
	caps = loop.GateApproval(caps, mode)
	caps = loop.TruncateResults(caps, ac.MaxToolResultLen) // 工具结果截断(Ring 0)

	// 会话记忆(store 提前构建:L4 召回钩子需要它)
	var store session.Store
	if ac.Memory.Window > 0 {
		var err error
		if store, err = session.New(ac.Memory.Store, ac.Memory.StoreConfig, ac.Memory.Window); err != nil {
			return nil, err
		}
	}

	// L1-L4 提示词拼装
	loopTpl, err := ac.LoopPrompt.Resolve(ctx, prompts)
	if err != nil {
		return nil, fmt.Errorf("resolve loop_prompt: %w", err)
	}
	personaTpl, err := ac.SystemPrompt.Resolve(ctx, prompts)
	if err != nil {
		return nil, fmt.Errorf("resolve system_prompt: %w", err)
	}
	layers := loop.PromptLayers{Loop: loopTpl.Text, Persona: personaTpl.Text}
	if topK := ac.Memory.AutoRecall.TopK; topK > 0 {
		layers.Memories = autoRecall(kv, store, ac.Memory.Window, topK)
	}

	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        m,
		Capabilities: caps,
		MaxSteps:     ac.MaxSteps,
		Modifier:     layers.Modifier(),
		Rewriter:     loop.Compactor(m, ac.Compaction),
	})
	if err != nil {
		return nil, err
	}

	enforcer, err := loop.NewStructuredEnforcer(ac.StructuredOutput)
	if err != nil {
		return nil, err
	}

	return agent.New(ac.Name, ac.Description, runner, m, agent.Options{
		Store: store, Window: ac.Memory.Window, Compaction: ac.Compaction,
		Structured: enforcer, Interactor: interactor,
	}), nil
}

// autoRecall 是 L4 自动召回钩子:每次模型调用前,用本轮用户输入
// 检索长期记忆与窗口外的会话历史,命中片段注入 system prompt 第四层。
func autoRecall(kv memory.KV, store session.Store, window, topK int) func(ctx context.Context) []string {
	return func(ctx context.Context) []string {
		query := runctx.Input(ctx)
		if query == "" {
			return nil
		}
		var out []string

		// 长期记忆命中
		if kv != nil {
			if hits, err := kv.Search(ctx, query, topK); err == nil {
				for k, v := range hits {
					out = append(out, fmt.Sprintf("长期记忆 %s: %s", k, v))
				}
			}
		}

		// 窗口外的会话历史命中(仅支持全量读取的后端)
		if fl, ok := store.(session.FullLoader); ok {
			all, err := fl.LoadAll(ctx, runctx.Session(ctx))
			if err == nil {
				raw, _ := agent.RawHistory(all)
				if window > 0 && len(raw) > window {
					older := raw[:len(raw)-window]
					for _, s := range session.SearchRelevant(older, query, topK) {
						out = append(out, "早前对话 "+s)
					}
				}
			}
		}
		return out
	}
}
