// Package config 提供整个应用的声明式定义与总装:
// 凭证、提示词源、能力源、skill、workflow、agent、serving、IM 通道,
// 一份 YAML 全部描述,Build 组装为可运行的 App。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

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
	"github.com/joewm9911/agent-kit/store"
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

// StoreInstance 是一个具名存储实例声明(agent 层,私有作用域)。
// 模块块的 store 槽用 cap://store/<kind>/<name> 引用它;换后端=改 type、
// 或声明另一实例把 store 指过去;跨 agent 共享=各自声明指向同一后端。
type StoreInstance struct {
	Name   string         `yaml:"name"`
	Kind   string         `yaml:"kind"` // session | memory | todo | result
	Type   string         `yaml:"type"` // inmemory | file | redis | ...(各自后端注册表)
	Config map[string]any `yaml:"config"`
	TTL    loop.Duration  `yaml:"ttl"` // 保留时长(todo/result),0=不过期
}

// RetrieverInstance 是一个具名召回器实例声明;session.recall 用
// cap://retriever/<kind>/<name> 引用它。
type RetrieverInstance struct {
	Name   string         `yaml:"name"`
	Kind   string         `yaml:"kind"` // session
	Type   string         `yaml:"type"` // bigram | vector | ...
	Config map[string]any `yaml:"config"`
}

// 四大上下文/记忆模块各自独立配置,各自的 store 槽用 cap://store/<kind>/
// <name> 引用 stores: 里的具名实例(或裸 type 作缺省简写)。四模块对称:
//   session → 会话短期记忆   memory → 长期记忆
//   todo    → 计划清单        digest → 大结果消化/暂存

// SessionConfig 是会话短期记忆模块:窗口、后端、轨迹详略、窗外召回、
// 上下文压缩。
type SessionConfig struct {
	Window      int            `yaml:"window"`       // 0 = 不启用会话记忆
	Store       string         `yaml:"store"`        // cap://store/session/<name> 或裸 type(inmemory/file/...)
	StoreConfig map[string]any `yaml:"store_config"` // 裸 type 时的就地配置
	// RecordTools 控制本轮工具轨迹随会话持久化的详略:
	// summary(默认)| full | off。off 退回只存问答(任务连续性受限)。
	RecordTools string `yaml:"record_tools"`
	// Recall 是窗口外会话历史的自动召回(L4):top_k>0 启用,retriever 指定
	// 检索策略(cap://retriever/session/<name> 或裸 type,缺省 bigram 词法)。
	Recall SessionRecall `yaml:"recall"`
	// Compaction 是上下文压缩(会话上下文管理):越过阈值滚动摘要。
	Compaction loop.CompactionConfig `yaml:"compaction"`
}

// SessionRecall 是窗外会话召回配置。
type SessionRecall struct {
	TopK            int            `yaml:"top_k"`
	Retriever       string         `yaml:"retriever"`
	RetrieverConfig map[string]any `yaml:"retriever_config"`
}

// MemoryConfig 是长期记忆模块:后端、工具挂载、作用域隔离、召回、seed。
type MemoryConfig struct {
	Store       string         `yaml:"store"`        // cap://store/memory/<name> 或裸 type
	StoreConfig map[string]any `yaml:"store_config"` // 裸 type 时的就地配置
	// Tools 挂载 memory_save/search 工具(长期记忆的读写入口)。
	Tools bool `yaml:"tools"`
	// Scope 是多用户隔离的作用域策略:write 对话写入落点(user 默认 |
	// shared | session),read 召回覆盖(缺省 [user, shared])。
	Scope MemoryScope `yaml:"scope"`
	// Recall 是长期记忆的自动召回(L4):top_k>0 启用,检索策略由后端决定。
	Recall struct {
		TopK int `yaml:"top_k"`
	} `yaml:"recall"`
	// Seed 装配期灌入共享池的知识条目(域共享知识的运维写入口)。
	Seed []MemorySeed `yaml:"seed"`
}

// MemoryScope 是长期记忆作用域。
type MemoryScope struct {
	Write string   `yaml:"write"`
	Read  []string `yaml:"read"`
}

// MemorySeed 是共享池 seed 条目。
type MemorySeed struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

// TodoConfig 是计划清单模块:开关 + 后端。
type TodoConfig struct {
	Enabled     *bool          `yaml:"enabled"` // 默认 true,显式 false 关闭
	Store       string         `yaml:"store"`   // cap://store/todo/<name> 或裸 type
	StoreConfig map[string]any `yaml:"store_config"`
}

// DigestConfig 是大结果消化/暂存模块:阈值 + 后端。
type DigestConfig struct {
	// Over>0 时,超过该 rune 数的工具结果先落暂存,由模型带当前任务提取
	// 要点后入上下文(附 read_result 取回指针),截断闸在其后兜底。
	Over        int            `yaml:"over"`
	Store       string         `yaml:"store"` // cap://store/result/<name> 或裸 type
	StoreConfig map[string]any `yaml:"store_config"`
}

// PromptConfig 是提示词分层模块:L1 框架规约 + L2 业务 persona,均支持
// 字面量或 {ref: cap://prompt/...} 引用。
type PromptConfig struct {
	System prompt.Value `yaml:"system"` // L2 业务 persona
	Loop   prompt.Value `yaml:"loop"`   // L1 框架规约覆盖(默认内置)
}

// CapabilitiesConfig 是能力选品 + 内置交互能力开关。
type CapabilitiesConfig struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
	AskUser *bool    `yaml:"ask_user"` // 内置交互能力(默认开,显式 false 关闭)
}

// ApprovalConfig 是审批治理模块:模式 + 参数级策略(规则 + 决策记忆);
// mode 之外的 remember/rules 内联自 loop.ApprovalPolicy。
type ApprovalConfig struct {
	Mode                string `yaml:"mode"` // interactive(默认) | auto | deny
	loop.ApprovalPolicy `yaml:",inline"`     // remember, rules
}

// AgentConfig 声明一个 agent(唯一主循环是 ReAct)。配置全部模块化,YAML
// 顶层各块各司其职:身份(name/description/model/max_steps)· prompt 提示词
// 分层 · capabilities 能力选品 · stores/retrievers 存储定义 · session/memory/
// todo/digest 四大上下文模块 · approval/budget/structured_output 治理边界。
type AgentConfig struct {
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Model       *ModelConfig `yaml:"model"`     // nil 用顶层 default_model
	MaxSteps    int          `yaml:"max_steps"` // 主循环步数上限

	Prompt       PromptConfig       `yaml:"prompt"`       // 提示词分层(L1 loop / L2 system)
	Capabilities CapabilitiesConfig `yaml:"capabilities"` // 能力选品 + 内置开关

	// Stores/Retrievers 是具名实例声明(仅定义);四大模块的 store/retriever
	// 槽用 cap://store/... · cap://retriever/... 引用它们(见 StoreInstance)。
	Stores     []StoreInstance     `yaml:"stores"`
	Retrievers []RetrieverInstance `yaml:"retrievers"`

	// 四大上下文/记忆模块,各自独立。
	Session SessionConfig `yaml:"session"`
	Memory  MemoryConfig  `yaml:"memory"`
	Todo    TodoConfig    `yaml:"todo"`
	Digest  DigestConfig  `yaml:"digest"`

	// MaxToolResultLen 是工具结果进入上下文的截断长度(rune):
	// 0 = 默认 8000,-1 = 关闭截断。防 MCP 等外部工具返回超大结果打爆窗口。
	MaxToolResultLen int `yaml:"max_tool_result_len"`

	// 治理边界(Ring 0,agent 独占、不被 namespace 覆盖):审批 + 预算 +
	// 结构化输出,三块各自顶层,与四大上下文模块并列。
	Approval         ApprovalConfig        `yaml:"approval"`
	Budget           loop.BudgetConfig     `yaml:"budget"`
	StructuredOutput loop.StructuredConfig `yaml:"structured_output"`
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
		a, err := buildAgent(ctx, ac, caps, prompts, m, cfg.Reliability, opts.Interactor, logger)
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
// resolveStoreRef 解析 store 槽引用:cap://store/<kind>/<name> → 具名实例
// 的 (type, config, ttl);裸字符串(inmemory/file/...)当作缺省简写直接
// 作 type 返回,存量零迁移。ref 为空返回空 type(上游按默认处理)。
func resolveStoreRef(ref string, stores []StoreInstance, wantKind string) (string, map[string]any, time.Duration, error) {
	if ref == "" || !strings.HasPrefix(ref, "cap://") {
		return ref, nil, 0, nil
	}
	r, err := capability.ParseRef(ref)
	if err != nil {
		return "", nil, 0, err
	}
	if r.Kind != "store" {
		return "", nil, 0, fmt.Errorf("%s: 不是 store 引用(kind=%s)", ref, r.Kind)
	}
	if r.Domain != wantKind {
		return "", nil, 0, fmt.Errorf("%s: store kind 为 %q,该槽需要 %q", ref, r.Domain, wantKind)
	}
	for _, s := range stores {
		if s.Name == r.Name {
			if s.Kind != wantKind {
				return "", nil, 0, fmt.Errorf("store 实例 %q 的 kind 为 %q,与槽 %q 不符", s.Name, s.Kind, wantKind)
			}
			return s.Type, s.Config, s.TTL.Std(), nil
		}
	}
	return "", nil, 0, fmt.Errorf("%s: 未声明名为 %q 的 store 实例", ref, r.Name)
}

// resolveRetrieverRef 解析 retriever 槽引用:cap://retriever/<kind>/<name>
// → 具名实例的 (type, config);裸字符串当作缺省简写。
func resolveRetrieverRef(ref string, retrievers []RetrieverInstance, wantKind string) (string, map[string]any, error) {
	if ref == "" || !strings.HasPrefix(ref, "cap://") {
		return ref, nil, nil
	}
	r, err := capability.ParseRef(ref)
	if err != nil {
		return "", nil, err
	}
	if r.Kind != "retriever" {
		return "", nil, fmt.Errorf("%s: 不是 retriever 引用(kind=%s)", ref, r.Kind)
	}
	if r.Domain != wantKind {
		return "", nil, fmt.Errorf("%s: retriever kind 为 %q,该槽需要 %q", ref, r.Domain, wantKind)
	}
	for _, rv := range retrievers {
		if rv.Name == r.Name {
			return rv.Type, rv.Config, nil
		}
	}
	return "", nil, fmt.Errorf("%s: 未声明名为 %q 的 retriever 实例", ref, r.Name)
}

// wireGlobalStore 解析并构建一个 KV 后端,注入进程级全局槽(todo/result)。
// ref 为空则不动(保持默认 inmemory)。
func wireGlobalStore(ref string, stores []StoreInstance, wantKind string, set func(store.KV, time.Duration)) error {
	if ref == "" {
		return nil
	}
	typ, conf, ttl, err := resolveStoreRef(ref, stores, wantKind)
	if err != nil {
		return err
	}
	kv, err := store.NewBackend(typ, conf)
	if err != nil {
		return fmt.Errorf("%s store backend: %w", wantKind, err)
	}
	set(kv, ttl)
	return nil
}

func buildAgent(ctx context.Context, ac *AgentConfig, caps []capability.Capability,
	prompts *prompt.Resolver, m model.ToolCallingChatModel,
	rel ReliabilityConfig, interactor runctx.Interactor, logger *slog.Logger) (*agent.Agent, error) {

	if logger == nil {
		logger = slog.Default()
	}

	if m == nil {
		return nil, fmt.Errorf("no model (declare model or default_model)")
	}

	// 进程级存储后端注入(todo 计划、result 大结果暂存):分布式多副本
	// 下须指向外置后端。键按 agent/会话隔离,同进程多 agent 共享同一后端
	// 无碰撞;未配置则保持默认 inmemory。
	if err := wireGlobalStore(ac.Todo.Store, ac.Stores, "todo", builtin.SetStore); err != nil {
		return nil, err
	}
	if err := wireGlobalStore(ac.Digest.Store, ac.Stores, "result", loop.SetResultBackend); err != nil {
		return nil, err
	}

	// 内置能力 + 审批闸门
	todoOn := ac.Todo.Enabled == nil || *ac.Todo.Enabled
	if todoOn {
		caps = append(caps, builtin.TodoCapabilities()...)
	}
	if ac.Capabilities.AskUser == nil || *ac.Capabilities.AskUser {
		caps = append(caps, builtin.AskUser())
	}
	// 召回配置:两路各自独立(session.recall / memory.recall),负值=关闭。
	sessK := ac.Session.Recall.TopK
	kvK := ac.Memory.Recall.TopK
	if sessK < 0 {
		sessK = 0
	}
	if kvK < 0 {
		kvK = 0
	}

	// 长期记忆后端:挂工具或启用长期召回任一都需要构建;
	// 工具面挂载仍只由 memory.tools 决定。
	scope := memory.ScopeConfig{Write: ac.Memory.Scope.Write, Read: ac.Memory.Scope.Read}
	var kv memory.KV
	if ac.Memory.Tools || kvK > 0 {
		ltType, ltConf, _, err := resolveStoreRef(ac.Memory.Store, ac.Stores, "memory")
		if err != nil {
			return nil, err
		}
		if ltConf == nil {
			ltConf = ac.Memory.StoreConfig // 裸 type 时沿用就地 config
		}
		if kv, err = memory.New(ltType, ltConf); err != nil {
			return nil, err
		}
		// 共享池 seed:域共享知识的运维写入口,装配期灌入(非对话路径)。
		for _, s := range ac.Memory.Seed {
			if err := kv.Put(ctx, memory.SharedScope, s.Key, s.Value); err != nil {
				return nil, fmt.Errorf("long_term seed: %w", err)
			}
		}
		if ac.Memory.Tools {
			caps = append(caps, memory.AsCapabilities(kv, scope)...)
		}
	}
	// Ring 0 闸门:超时(最内)→ 截断 → 审批(最外,批准等待不占超时)。
	// 审批运行态(模式+参数级策略+决策记忆)与预算门闸由 agent 每次
	// 运行装入 ctx,对 skill 内部同样生效。
	mode := loop.ApprovalMode(ac.Approval.Mode)
	if mode == "" {
		mode = loop.ApprovalInteractive
	}
	approval, err := loop.NewApprovalState(mode, ac.Approval.ApprovalPolicy)
	if err != nil {
		return nil, err
	}
	if ac.Digest.Over > 0 {
		caps = append(caps, loop.ReadResult()) // 消化结果的原文取回
	}
	caps = loop.TimeoutTools(caps, rel.ToolTimeout.Std())
	caps = loop.DigestResults(caps, m, ac.Digest.Over) // 大结果消化
	caps = loop.TruncateResults(caps, ac.MaxToolResultLen)           // 工具结果截断(Ring 0)
	caps = suspend.DurableEffects(caps)                              // 效果日志(挂起恢复的重放不二次执行)
	caps = loop.GateApprovalCtx(caps)
	caps = loop.ControlTools(caps) // 中断/插话检查点(审批之外:中断时不再询问)
	if todoOn {
		caps = builtin.NudgeTools(caps) // 计划卡住提醒(harness 强制纪律)
	}
	caps = loop.RecordTools(caps) // 轨迹记录(最外层:记模型实际看到的)

	// 会话记忆
	var sessStore session.Store
	if ac.Session.Window > 0 {
		sType, sConf, _, err := resolveStoreRef(ac.Session.Store, ac.Stores, "session")
		if err != nil {
			return nil, err
		}
		if sConf == nil {
			sConf = ac.Session.StoreConfig // 裸 type 时沿用就地 config
		}
		if sessStore, err = session.New(sType, sConf, ac.Session.Window); err != nil {
			return nil, err
		}
		// FullLoader 是滚动摘要与窗外召回的前提:自定义后端没实现时
		// 这两项能力静默消失——装配期把降级喊出来,不让它悄悄发生。
		if _, ok := sessStore.(session.FullLoader); !ok {
			if ac.Session.Compaction.Enabled() || sessK > 0 {
				logger.Warn("session store lacks FullLoader: rolling summary and beyond-window recall are DISABLED",
					slog.String("agent", ac.Name), slog.String("store", ac.Session.Store))
			}
		}
		// 窗口必须容得下摘要视图:否则滚动摘要(+锚定)会被窗口裁剪
		// 静默切掉,跨轮记忆凭空消失。+2 = 摘要 + 锚定两条合成消息。
		if ac.Session.Compaction.Enabled() && ac.Session.Window < ac.Session.Compaction.Keep()+2 {
			return nil, fmt.Errorf("memory.window (%d) must be >= compaction keep_recent+2 (%d), or the rolling summary gets trimmed away",
				ac.Session.Window, ac.Session.Compaction.Keep()+2)
		}
	}

	// 摘要提示词(内容策略可配置,归并指令框架追加):装配期解析锁版本
	if err := ac.Session.Compaction.ResolvePrompt(ctx, prompts); err != nil {
		return nil, err
	}

	// L1-L4 提示词拼装
	loopTpl, err := ac.Prompt.Loop.Resolve(ctx, prompts)
	if err != nil {
		return nil, fmt.Errorf("resolve prompt.loop: %w", err)
	}
	personaTpl, err := ac.Prompt.System.Resolve(ctx, prompts)
	if err != nil {
		return nil, fmt.Errorf("resolve prompt.system: %w", err)
	}
	layers := loop.PromptLayers{Loop: loopTpl.Text, Persona: personaTpl.Text}
	if todoOn {
		layers.Plan = builtin.PlanSection // 计划每轮注入消息尾部(harness 强制可见)
	} else if layers.Loop == "" {
		layers.Loop = loop.DefaultLoopPromptNoTodo // 关闭 todo:提示词不承诺不存在的工具
	}
	if sessK > 0 || kvK > 0 {
		var retr session.Retriever
		if sessK > 0 {
			var err error // 装配期解析检索器名,未注册即拒绝(fail fast)
			rType, rConf, rerr := resolveRetrieverRef(ac.Session.Recall.Retriever, ac.Retrievers, "session")
			if rerr != nil {
				return nil, rerr
			}
			if rConf == nil {
				rConf = ac.Session.Recall.RetrieverConfig
			}
			if retr, err = session.NewRetriever(rType, rConf); err != nil {
				return nil, err
			}
		}
		layers.Memories = autoRecall(kv, scope, retr, ac.Session.Window, sessK, kvK)
	}

	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        m,
		Capabilities: caps,
		MaxSteps:     ac.MaxSteps,
		Modifier:     layers.Modifier(),
		Rewriter:     loop.Compactor(m, ac.Session.Compaction),
	})
	if err != nil {
		return nil, err
	}

	enforcer, err := loop.NewStructuredEnforcer(ac.StructuredOutput)
	if err != nil {
		return nil, err
	}

	record := loop.RecordMode(ac.Session.RecordTools)
	if record == "" {
		record = loop.RecordSummary
	}
	return agent.New(ac.Name, ac.Description, runner, m, agent.Options{
		Store: sessStore, Window: ac.Session.Window, Compaction: ac.Session.Compaction,
		Structured: enforcer, Interactor: interactor,
		Approval: approval, Budget: loop.NewBudgetGate(ac.Budget),
		RecordTools: record,
	}), nil
}

// autoRecall 是 L4 自动召回钩子:每次模型调用前,用本轮用户输入
// 检索长期记忆(KV,策略在后端)与窗口外的会话历史(策略在注册的
// Retriever),两路独立配置,命中片段注入消息尾部第四层。
// 会话历史来自 agent 本轮已加载的全量记录(ctx 共享,一轮只读一次
// store);同轮多次模型调用的查询不变,结果按轮 memo,不重复检索。
func autoRecall(kv memory.KV, scope memory.ScopeConfig, retr session.Retriever, window, sessK, kvK int) func(ctx context.Context) []string {
	var mu sync.Mutex
	memo := map[string]struct {
		input  string
		result []string
	}{}

	return func(ctx context.Context) []string {
		query := runctx.Input(ctx)
		if query == "" {
			return nil
		}
		sess := runctx.Session(ctx)
		mu.Lock()
		if m, ok := memo[sess]; ok && m.input == query {
			mu.Unlock()
			return m.result
		}
		mu.Unlock()

		var out []string
		// 长期记忆命中(kvK 路)
		if kv != nil && kvK > 0 {
			if hits, err := kv.Search(ctx, scope.ReadScopes(ctx), query, kvK); err == nil {
				for k, v := range hits {
					out = append(out, fmt.Sprintf("长期记忆 %s: %s", k, v))
				}
			}
		}
		// 窗口外的会话历史命中(sessK 路;本轮加载的全量记录,不回读 store)
		if retr != nil && sessK > 0 {
			if all := loop.TurnHistory(ctx); len(all) > 0 {
				raw, _ := agent.RawHistory(all)
				if window > 0 && len(raw) > window {
					older := raw[:len(raw)-window]
					for _, s := range retr.Retrieve(ctx, older, query, sessK) {
						out = append(out, "早前对话 "+s)
					}
				}
			}
		}

		mu.Lock()
		if len(memo) > 1024 { // 粗粒度防泄漏
			memo = map[string]struct {
				input  string
				result []string
			}{}
		}
		memo[sess] = struct {
			input  string
			result []string
		}{query, out}
		mu.Unlock()
		return out
	}
}
