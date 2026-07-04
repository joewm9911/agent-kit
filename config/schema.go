// schema.go 收口全部配置模型定义(YAML 结构):app / agent / namespace /
// component 的声明式 schema 都在这里;各层的“组装”逻辑分见 config.go(单
// 文件)、app.go(多文件)、agent.go(agent 装配)、namespace.go(ns 装配)。
package config

import (
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/skill"
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

// PromptsConfig 是 app 级提示词供给模块:供给源 + 默认版本标签。
type PromptsConfig struct {
	Sources      []PromptSourceConfig `yaml:"sources"`
	DefaultLabel string               `yaml:"default_label"`
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

// DigestConfig 是「大工具结果进上下文前的处理」模块:同一环节两道闸——
// over 触发消化、truncate 硬截断兜底。
type DigestConfig struct {
	// Over>0 时,超过该 rune 数的工具结果先落暂存,由模型带当前任务提取
	// 要点后入上下文(附 read_result 取回指针),截断闸在其后兜底。
	Over int `yaml:"over"`
	// Truncate 是所有工具结果进上下文的硬截断长度(rune):0=默认 8000,
	// -1=关闭。作用于每个工具结果(不只被消化的),是消化之后的最终兜底。
	Truncate    int            `yaml:"truncate"`
	Store       string         `yaml:"store"` // cap://store/result/<name> 或裸 type
	StoreConfig map[string]any `yaml:"store_config"`
}

// LoopConfig 是主循环(ReAct)控制。
type LoopConfig struct {
	MaxSteps int `yaml:"max_steps"` // 主循环迭代上限(外层兜底,是否完成由模型自然表达)
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
	Mode                string           `yaml:"mode"` // interactive(默认) | auto | deny
	loop.ApprovalPolicy `yaml:",inline"` // remember, rules
}

// AgentConfig 声明一个 agent(唯一主循环是 ReAct)。配置全部按执行环节
// 模块化,YAML 顶层各块各司其职:身份(name/description/model)· prompt
// 提示词分层 · capabilities 能力选品 · loop 主循环 · stores/retrievers 存储
// 定义 · session/memory/todo/digest 四大上下文模块 · approval/budget/
// structured_output 治理边界。
type AgentConfig struct {
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Model       *ModelConfig `yaml:"model"` // nil 用顶层 default_model

	Prompt       PromptConfig       `yaml:"prompt"`       // 提示词分层(L1 loop / L2 system)
	Capabilities CapabilitiesConfig `yaml:"capabilities"` // 能力选品 + 内置开关
	Loop         LoopConfig         `yaml:"loop"`         // 主循环控制(max_steps)

	// Stores/Retrievers 是具名实例声明(仅定义);四大模块的 store/retriever
	// 槽用 cap://store/... · cap://retriever/... 引用它们(见 StoreInstance)。
	Stores     []StoreInstance     `yaml:"stores"`
	Retrievers []RetrieverInstance `yaml:"retrievers"`

	// 四大上下文/记忆模块,各自独立(digest 含工具结果截断 truncate)。
	Session SessionConfig `yaml:"session"`
	Memory  MemoryConfig  `yaml:"memory"`
	Todo    TodoConfig    `yaml:"todo"`
	Digest  DigestConfig  `yaml:"digest"`

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

	Prompts PromptsConfig `yaml:"prompts"`

	Sources []SourceConfig `yaml:"sources"`
	Catalog CatalogConfig  `yaml:"catalog"`

	DefaultModel *ModelConfig `yaml:"default_model"`
	// Reliability 是全局可靠性策略(Ring 0):模型瞬时错误重试、
	// 工具单次调用超时。零值即启用默认策略。
	Reliability ReliabilityConfig `yaml:"reliability"`

	// Namespaces 是三层结构的主路径:tools(ns 内共享)→ components
	// (执行单元声明)→ skills(对外产品,唯一进目录的编排单元)。
	Namespaces []NamespaceConfig `yaml:"namespaces"`

	// Skills 是平铺声明的兼容路径,新配置建议用 namespaces。
	// (workflow 不再单列:顺序编排用 namespace 的 engine: workflow component/skill。)
	Skills []*skill.Declaration `yaml:"skills"`
	Agents []AgentConfig        `yaml:"agents"`

	Serving  ServingConfig   `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`

	Suspend SuspendConfig `yaml:"suspend"`

	Observability ObservabilityConfig `yaml:"observability"`
}

// Defaults 是可被下层重定义的执行参数默认值(治理策略不在此列)。
// 全部为指针/可判空字段:只有显式写了的键参与覆盖,零值不污染链条。
type Defaults struct {
	// Model 是 component 未声明专属模型时的默认(component → ns → agent → app)。
	Model *ModelConfig `yaml:"model"`
	// MaxSteps 是 component 内部循环的默认步数上限。
	MaxSteps *int `yaml:"max_steps"`
	// Compaction 是 component 内部循环的默认压缩策略。
	Compaction *loop.CompactionConfig `yaml:"compaction"`
	// ToolTimeout/Retry 是 component 内部工具面与专属模型的可靠性默认。
	ToolTimeout *loop.Duration    `yaml:"tool_timeout"`
	Retry       *loop.RetryConfig `yaml:"retry"`
	// StepTimeout/StepRetry 是编排步骤未声明 timeout/retry 时的默认。
	StepTimeout *loop.Duration `yaml:"step_timeout"`
	StepRetry   *int           `yaml:"step_retry"`
	// DigestOver 是 component 内部工具面的大结果消化阈值默认。
	DigestOver *int `yaml:"digest_over"`
}

// AppConfig 是应用级入口(app.yaml):进程级资源与接线板。
type AppConfig struct {
	Secrets SecretsConfig `yaml:"secrets"`

	Prompts PromptsConfig `yaml:"prompts"`

	// Sources 是全局兼容源(直挂 agent 工具面的通用工具,如 fs)。
	Sources []SourceConfig `yaml:"sources"`
	Catalog CatalogConfig  `yaml:"catalog"`

	DefaultModel *ModelConfig      `yaml:"default_model"`
	Reliability  ReliabilityConfig `yaml:"reliability"`

	// Agents 是 agent 文件路径列表,相对 app.yaml 所在目录。
	Agents []string `yaml:"agents"`

	Serving  ServingConfig   `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`
	Suspend  SuspendConfig   `yaml:"suspend"`

	Observability ObservabilityConfig `yaml:"observability"`
}

// AgentFile 是 agent 维度的配置文件(agents/<name>.yaml)。
type AgentFile struct {
	AgentConfig `yaml:",inline"`
	// Namespaces 是关联的 namespace 文件路径(相对本文件),自动挂载
	// 其全部导出 skill;capabilities.exclude 可屏蔽个别。
	Namespaces []string `yaml:"namespaces"`
	// Defaults 是 agent 级执行参数默认值,namespace/component 未声明时回落至此。
	Defaults Defaults `yaml:"defaults"`
}

// NamespaceFile 是 namespace 维度的配置文件(namespaces/<name>.yaml)。
type NamespaceFile struct {
	NamespaceConfig `yaml:",inline"`
	// Defaults 是 namespace 级执行参数默认值,覆盖 agent 级。
	Defaults Defaults `yaml:"defaults"`
}

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
	// Todo 给内部循环挂调用级临时清单(仅 react;调用结束即弃)。
	// 默认关——component 长到需要计划通常是"该拆成结构"的信号,
	// 这是给确实拆不动的研究型长循环的例外通道。
	Todo bool `yaml:"todo"`

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
