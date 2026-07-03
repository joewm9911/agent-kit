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

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/builtin"
	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/channel"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/memory"
	"github.com/joewm9911/agent-kit/observe"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/registry"
	"github.com/joewm9911/agent-kit/runctx"
	"github.com/joewm9911/agent-kit/secrets"
	"github.com/joewm9911/agent-kit/serving"
	"github.com/joewm9911/agent-kit/session"
	"github.com/joewm9911/agent-kit/skill"
	"github.com/joewm9911/agent-kit/source"
	"github.com/joewm9911/agent-kit/suspend"
	"github.com/joewm9911/agent-kit/workflow"
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
	Model        *ModelConfig `yaml:"model"`         // nil 用顶层 default_model
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
		// RecordTools 控制本轮工具轨迹随会话持久化的详略:
		// summary(默认)| full | off。off 退回只存问答(任务连续性受限)。
		RecordTools string `yaml:"record_tools"`
	} `yaml:"memory"`

	// MaxToolResultLen 是工具结果进入上下文的截断长度(rune):
	// 0 = 默认 8000,-1 = 关闭截断。防 MCP 等外部工具返回超大结果打爆窗口。
	MaxToolResultLen int `yaml:"max_tool_result_len"`

	// ContextHygiene 是上下文卫生策略:digest_over > 0 时,超过该
	// rune 数的工具结果先落 run 级暂存,由模型带当前任务提取要点后
	// 入上下文(附 read_result 取回指针)——搜索、捞日志等大数据量
	// 工具不再污染上下文。截断闸仍在其后兜底。
	ContextHygiene struct {
		DigestOver int `yaml:"digest_over"`
	} `yaml:"context_hygiene"`

	Budget           loop.BudgetConfig     `yaml:"budget"`
	Compaction       loop.CompactionConfig `yaml:"compaction"`
	StructuredOutput loop.StructuredConfig `yaml:"structured_output"`
	// Approval:interactive(默认)| auto | deny。
	Approval string `yaml:"approval"`
	// ApprovalPolicy 是参数级审批规则与决策记忆(interactive 模式下生效)。
	ApprovalPolicy loop.ApprovalPolicy `yaml:"approval_policy"`
}

// ReliabilityConfig 是全局可靠性策略。
type ReliabilityConfig struct {
	// ModelRetry 是模型调用的瞬时错误重试,零值 = 默认 3 次尝试。
	ModelRetry loop.RetryConfig `yaml:"model_retry"`
	// ToolTimeout 是工具单次调用超时,0 = 默认 5m,负 = 关闭。
	ToolTimeout loop.Duration `yaml:"tool_timeout"`
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

// SecretsConfig 声明凭证 provider。
type SecretsConfig struct {
	Provider string         `yaml:"provider"` // env(默认)| file
	Config   map[string]any `yaml:"config"`
}

// CatalogConfig 是目录准入配置。
type CatalogConfig struct {
	MaxRisk string `yaml:"max_risk"` // 准入上限,默认 mutating(dangerous 不入目录)
}

// ServingConfig 是 Gateway 配置。
type ServingConfig struct {
	Addr string `yaml:"addr"`
}

// SuspendConfig 启用 IM 通道的持久化挂起:ask_user/审批等待落盘,
// 跨小时/跨天/跨进程重启均可恢复;未配置时为进程内阻塞等待。
type SuspendConfig struct {
	Dir string `yaml:"dir"` // 挂起状态目录,非空即启用
}

// ObservabilityConfig 是观测配置。
type ObservabilityConfig struct {
	Log            bool   `yaml:"log"`
	TrajectoryPath string `yaml:"trajectory_path"`
}

// Config 是应用的完整声明(单文件形态,兼容路径;
// 多文件形态见 LoadApp:app.yaml + 每 agent/namespace 一个文件)。
type Config struct {
	Secrets SecretsConfig `yaml:"secrets"`

	PromptSources      []PromptSourceConfig `yaml:"prompt_sources"`
	PromptDefaultLabel string               `yaml:"prompt_default_label"`

	Sources []SourceConfig `yaml:"sources"`
	Catalog CatalogConfig  `yaml:"catalog"`

	DefaultModel *ModelConfig `yaml:"default_model"`
	// Reliability 是全局可靠性策略(Ring 0):模型瞬时错误重试、
	// 工具单次调用超时。零值即启用默认策略。
	Reliability ReliabilityConfig `yaml:"reliability"`

	// Namespaces 是三层结构的主路径:tools(ns 内共享)→ components
	// (执行单元声明)→ skills(对外产品,唯一进目录的编排单元)。
	Namespaces []NamespaceConfig `yaml:"namespaces"`

	// Skills/Workflows 是平铺声明的兼容路径,新配置建议用 namespaces。
	Skills    []*skill.Declaration `yaml:"skills"`
	Workflows []workflow.Config    `yaml:"workflows"`
	Agents    []AgentConfig        `yaml:"agents"`

	Serving  ServingConfig   `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`

	Suspend SuspendConfig `yaml:"suspend"`

	Observability ObservabilityConfig `yaml:"observability"`
}

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
	switch head.Secrets.Provider {
	case "", "env":
		return secrets.Env{}, nil
	case "file":
		p, _ := head.Secrets.Config["path"].(string)
		return secrets.NewFile(p)
	default:
		return nil, fmt.Errorf("unknown secrets provider %q", head.Secrets.Provider)
	}
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

	// 默认模型(skill/workflow 与未声明 model 的 agent 使用)。
	// 统一套 Ring 0 中间件:瞬时错误重试 + 预算(门闸经 ctx 生效,
	// skill/component 内部调用同样计入调用方会话预算)。
	var defaultModel model.ToolCallingChatModel
	if cfg.DefaultModel != nil {
		var err error
		if defaultModel, err = registry.BuildModel(ctx, cfg.DefaultModel.Provider, cfg.DefaultModel.Config); err != nil {
			return nil, fmt.Errorf("default_model: %w", err)
		}
		defaultModel = loop.BudgetModel(loop.RetryModel(defaultModel, cfg.Reliability.ModelRetry))
	}

	// 4. skills:装配后入目录,供 agent 选品
	for _, decl := range cfg.Skills {
		c, err := skill.Build(ctx, decl, skill.Deps{
			Catalog: catalog, Prompts: prompts, DefaultModel: defaultModel,
			ToolTimeout: cfg.Reliability.ToolTimeout.Std(), Retry: cfg.Reliability.ModelRetry,
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

	// 5b. namespaces:三层结构装配(tools → components → skills),
	// 只有 skills 进全局目录;声明顺序决定跨 ns 引用的可见性。
	for i := range cfg.Namespaces {
		err := buildNamespace(ctx, &cfg.Namespaces[i], nsDeps{
			global: catalog, prompts: prompts, defaultModel: defaultModel,
			maxRisk: maxRisk, toolTimeout: cfg.Reliability.ToolTimeout,
			retry: cfg.Reliability.ModelRetry, logger: logger,
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
		m, err := agentModel(ctx, ac.Model, nil, defaultModel, cfg.Reliability)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", ac.Name, err)
		}
		a, err := buildAgent(ctx, ac, caps, prompts, m, cfg.Reliability, opts.Interactor)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", ac.Name, err)
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
		if cfg.Suspend.Dir != "" {
			store, err := suspend.NewFileStore(cfg.Suspend.Dir)
			if err != nil {
				return nil, fmt.Errorf("suspend: %w", err)
			}
			dispatcher.EnableSuspend(store)
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

// agentModel 解析 agent 的模型:own(agent 自己声明)→ def(agent 级
// 默认,来自 defaults 链)→ fallback(app 默认,已包装)。前两者在此
// 套 Ring 0 中间件(重试+预算)。
func agentModel(ctx context.Context, own, def *ModelConfig,
	fallback model.ToolCallingChatModel, rel ReliabilityConfig) (model.ToolCallingChatModel, error) {

	mc := own
	if mc == nil {
		mc = def
	}
	if mc == nil {
		if fallback == nil {
			return nil, fmt.Errorf("no model (declare model or default_model)")
		}
		return fallback, nil
	}
	m, err := registry.BuildModel(ctx, mc.Provider, mc.Config)
	if err != nil {
		return nil, err
	}
	return loop.BudgetModel(loop.RetryModel(m, rel.ModelRetry)), nil
}

// buildAgent 用已选品的能力面与已解析的模型装配 agent。
// 选品与模型解析由调用方完成(单文件路径按 include 选品;多文件路径
// 自动挂载关联 namespace 的全部导出 skill)。
func buildAgent(ctx context.Context, ac *AgentConfig, caps []capability.Capability,
	prompts *prompt.Resolver, m model.ToolCallingChatModel,
	rel ReliabilityConfig, interactor runctx.Interactor) (*agent.Agent, error) {

	if m == nil {
		return nil, fmt.Errorf("no model (declare model or default_model)")
	}

	// 内置能力 + 审批闸门
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
	// Ring 0 闸门:超时(最内)→ 截断 → 审批(最外,批准等待不占超时)。
	// 审批运行态(模式+参数级策略+决策记忆)与预算门闸由 agent 每次
	// 运行装入 ctx,对 skill 内部同样生效。
	mode := loop.ApprovalMode(ac.Approval)
	if mode == "" {
		mode = loop.ApprovalInteractive
	}
	approval, err := loop.NewApprovalState(mode, ac.ApprovalPolicy)
	if err != nil {
		return nil, err
	}
	if ac.ContextHygiene.DigestOver > 0 {
		caps = append(caps, loop.ReadResult()) // 消化结果的原文取回
	}
	caps = loop.TimeoutTools(caps, rel.ToolTimeout.Std())
	caps = loop.DigestResults(caps, m, ac.ContextHygiene.DigestOver) // 大结果消化
	caps = loop.TruncateResults(caps, ac.MaxToolResultLen)           // 工具结果截断(Ring 0)
	caps = suspend.DurableEffects(caps)                    // 效果日志(挂起恢复的重放不二次执行)
	caps = loop.GateApprovalCtx(caps)
	caps = loop.ControlTools(caps) // 中断/插话检查点(审批之外:中断时不再询问)
	caps = loop.RecordTools(caps)  // 轨迹记录(最外层:记模型实际看到的)

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

	record := loop.RecordMode(ac.Memory.RecordTools)
	if record == "" {
		record = loop.RecordSummary
	}
	return agent.New(ac.Name, ac.Description, runner, m, agent.Options{
		Store: store, Window: ac.Memory.Window, Compaction: ac.Compaction,
		Structured: enforcer, Interactor: interactor,
		Approval: approval, Budget: loop.NewBudgetGate(ac.Budget),
		RecordTools: record,
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
