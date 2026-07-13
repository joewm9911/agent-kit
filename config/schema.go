// schema.go 收口全部配置模型定义(YAML 结构):app / agent / namespace /
// component 的声明式 schema 都在这里;各层的“组装”逻辑分见 config.go(单
// 文件)、app.go(多文件)、agent.go(agent 装配)、namespace.go(ns 装配)。
//
// 执行画像(model/loop/reliability/digest/steps)是四层共用的一套 Profile
// (见 profile.go),各层内嵌;治理边界(approval/budget/structured_output)
// 与会话状态(session/memory/todo)是 agent 专属、可由 app 设默认。
package config

import (
	"io/fs"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
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

// StoreInstance 是一个具名存储实例声明(app 或 agent 层)。模块块的 store
// 槽用 cap://store/<kind>/<name> 引用它;换后端=改 type、或声明另一实例把
// store 指过去;跨 agent 共享=在 app 层声明一次,各 agent 引用同名实例。
type StoreInstance struct {
	Name   string         `yaml:"name"`
	Kind   string         `yaml:"kind"` // session | memory | todo | result | suspend | budget | approval
	Type   string         `yaml:"type"` // inmemory | file | redis | ...(各自后端注册表)
	Config map[string]any `yaml:"config"`
	TTL    loop.Duration  `yaml:"ttl"` // 保留时长(todo/result/approval/budget),0=不过期
}

// RetrieverInstance 是一个具名召回器实例声明;session.recall 用
// cap://retriever/<kind>/<name> 引用它。
type RetrieverInstance struct {
	Name   string         `yaml:"name"`
	Kind   string         `yaml:"kind"` // session
	Type   string         `yaml:"type"` // bigram | vector | ...
	Config map[string]any `yaml:"config"`
}

// SessionConfig 是会话短期记忆模块:窗口、后端、轨迹详略、窗外召回。
// (上下文压缩 compaction 归执行画像 loop.compaction,主 loop 与 component
// 共用同一机制,见 LoopProfile。)
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
}

// isZero 报告 SessionConfig 是否未声明(用于 app→agent 整块降级)。
func (s SessionConfig) isZero() bool {
	return s.Window == 0 && s.Store == "" && s.StoreConfig == nil &&
		s.RecordTools == "" && s.Recall.TopK == 0 && s.Recall.Retriever == "" &&
		s.Recall.RetrieverConfig == nil
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
	// ExposeTools 挂载 memory_save/search 工具(长期记忆的读写入口)。
	// 原名 tools 与"工具列表"惯用词冲突,已改名(旧键装配期报错)。
	ExposeTools bool  `yaml:"expose_tools"`
	ToolsLegacy *bool `yaml:"tools"` // 已废弃:改 expose_tools(报错指路)
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

// isZero 报告 MemoryConfig 是否未声明(用于 app→agent 整块降级)。
func (m MemoryConfig) isZero() bool {
	return m.Store == "" && m.StoreConfig == nil && !m.ExposeTools && m.ToolsLegacy == nil &&
		m.Scope.Write == "" && m.Scope.Read == nil && m.Recall.TopK == 0 && m.Seed == nil
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

// isZero 报告 TodoConfig 是否未声明(用于 app→agent 整块降级)。
func (t TodoConfig) isZero() bool {
	return t.Enabled == nil && t.Store == "" && t.StoreConfig == nil
}

// PromptConfig 是提示词分层模块:L1 框架规约 + L2 业务 persona,均支持
// 标量:字面量或 cap://prompt/ 前缀引用。
type PromptConfig struct {
	System prompt.Value `yaml:"system"` // L2 业务 persona
	Loop   prompt.Value `yaml:"loop"`   // L1 框架规约覆盖(默认内置)
}

// CapabilitiesConfig 是能力选品 + 内置交互能力开关。
type CapabilitiesConfig struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
	AskUser *bool    `yaml:"ask_user"` // 内置交互能力(默认开,显式 false 关闭)
	// GoalCheck 开启目标达成核对(U4.1):多步任务收尾前强制一次"对照原始
	// 目标逐条自查"的重生成。**默认关**——真机 A/B 显示强模型 + 中等难度
	// 任务上,强制重生成可能丢内容、净负;价值在弱模型/更难/高风险任务上,
	// 待 eval 定位后再考虑放开默认。显式 true 开启。
	GoalCheck *bool `yaml:"goal_check"`
}

// ApprovalConfig 是审批治理模块:模式 + 参数级策略(规则 + 决策记忆);
// mode 之外的 remember/rules 内联自 loop.ApprovalPolicy。store 槽指定
// 决策记忆后端(cap://store/approval/<name> 或裸 type):不配留进程内,
// 配 redis 则"总是允许/拒绝"跨副本生效。
type ApprovalConfig struct {
	Mode                string           `yaml:"mode"` // interactive(默认) | auto | deny
	loop.ApprovalPolicy `yaml:",inline"` // remember, rules
	Store               string           `yaml:"store"`
	StoreConfig         map[string]any   `yaml:"store_config"`
}

// isZero 报告 ApprovalConfig 是否未声明(用于 app→agent 整块降级)。
func (a ApprovalConfig) isZero() bool {
	return a.Mode == "" && !a.Remember && len(a.Rules) == 0 && a.Store == "" && a.StoreConfig == nil
}

// BudgetConfig 是预算治理模块:上限(内联自 loop.BudgetConfig)+ 账目
// 后端。store 槽(cap://store/budget/<name> 或裸 type)不配时账目留在
// 进程内(单副本);配 redis 则同一会话跨副本共用一份账目,预算是真正
// 的分布式硬上限。
type BudgetConfig struct {
	loop.BudgetConfig `yaml:",inline"` // max_model_calls, max_tokens
	Store             string           `yaml:"store"`
	StoreConfig       map[string]any   `yaml:"store_config"`
}

// isZero 报告 BudgetConfig 是否未声明(用于 app→agent 整块降级)。
func (b BudgetConfig) isZero() bool {
	return b.MaxModelCalls == 0 && b.MaxTokens == 0 && b.Store == "" && b.StoreConfig == nil
}

// AgentConfig 声明一个 agent(唯一主循环是 ReAct)。执行画像(model/loop/
// reliability/digest/steps)内嵌自 Profile,是主循环设置、兼作其 component 的
// 通用默认;会话状态(session/memory/todo)是主循环专属;治理边界(approval/
// budget/structured_output)是 agent 独占的 Ring 0 安全边界。
type AgentConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// 执行画像 A 类(model/loop/reliability/digest/steps),agent 自己这一层:
	// 既配主循环,也作为其 component 的降级源。
	Profile `yaml:",inline"`

	Prompt       PromptConfig       `yaml:"prompt"`       // 提示词分层(L1 loop / L2 system)
	Capabilities CapabilitiesConfig `yaml:"capabilities"` // 能力选品 + 内置开关

	// Stores/Retrievers 是具名实例声明(仅定义);模块块的 store/retriever
	// 槽用 cap://store/... · cap://retriever/... 引用它们(见 StoreInstance)。
	Stores     []StoreInstance     `yaml:"stores"`
	Retrievers []RetrieverInstance `yaml:"retrievers"`

	// 会话状态(主循环专属,component 无状态调用没有):
	Session SessionConfig `yaml:"session"`
	Memory  MemoryConfig  `yaml:"memory"`
	Todo    TodoConfig    `yaml:"todo"`

	// 治理边界(Ring 0,agent 独占、不被 namespace 覆盖):审批 + 预算 +
	// 结构化输出,三块各自顶层。
	Approval         ApprovalConfig        `yaml:"approval"`
	Budget           BudgetConfig          `yaml:"budget"`
	StructuredOutput loop.StructuredConfig `yaml:"structured_output"`
}

// ChannelConfig 声明一个 IM 通道绑定。
type ChannelConfig struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	Agent          string `yaml:"agent"`
	SessionMapping string `yaml:"session_mapping"` // chat | chat_user
	ReplyMode      string `yaml:"reply_mode"`      // text | card | stream(无装饰器时的默认策略)
	// Placeholder 是 processing 占位文案(空 = 内置英文默认「⏳ Working…」)。
	Placeholder string `yaml:"placeholder"`
	// Texts 覆盖面向用户的文案(键 = serving.Texts 字段的 snake_case,如
	// placeholder/stopped/approval;空字段回落英文默认)。IM 部署在此配
	// 本地化文案。
	Texts map[string]string `yaml:"texts"`
	// Decorator/OnProgress 按名引用代码注册的扩展(serving.RegisterDecorator /
	// RegisterProgressHandler),装配期查名 fail fast。
	Decorator  string         `yaml:"decorator"`
	OnProgress string         `yaml:"on_progress"`
	Config     map[string]any `yaml:"config"`
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

// SuspendConfig 启用持久化挂起(IM 通道与 HTTP /messages 共用同一
// 后端):ask_user/审批等待持久化,跨小时/跨天/跨进程重启均可恢复;
// 未配置时 IM 为进程内阻塞等待,HTTP 用装配时注入的 Interactor。
// 后端收敛到 store.KV:store 写裸 type(file/redis/...)或
// cap://store/suspend/<name> 引用具名实例(多文件 app 层);
// dir 是 file 后端的简写(等价 store: file + store_config: {dir: ...})。
type SuspendConfig struct {
	Dir         string         `yaml:"dir"`
	Store       string         `yaml:"store"`
	StoreConfig map[string]any `yaml:"store_config"`
	// TTL 是挂起记录(轮次/交互/效果日志)的过期时长,缺省 168h(7 天):
	// 无人应答的挂起不该在后端永久堆积。具名 store 实例自带的 ttl 优先。
	TTL loop.Duration `yaml:"ttl"`
}

// ObservabilityConfig 是观测配置。
type ObservabilityConfig struct {
	Log            bool   `yaml:"log"`
	TrajectoryPath string `yaml:"trajectory_path"`
}

// Config 是应用的完整声明(单文件形态,兼容路径;多文件形态见 LoadApp:
// app.yaml + 每 agent/namespace 一个文件)。顶层执行画像(model/loop/
// reliability/digest/steps)内嵌自 Profile,是所有 agent/component 的基线。
type Config struct {
	// root 是加载它的只读资源 FS(由 Load 设置;prompt 等只读子资源同源
	// 解析)。非 YAML 字段——直接构造 Config 的调用方(测试)可为 nil,
	// prompt file 源回落 os.DirFS。
	root fs.FS

	Secrets SecretsConfig `yaml:"secrets"`

	Prompts PromptsConfig `yaml:"prompts"`

	Sources []SourceConfig `yaml:"sources"`
	Catalog CatalogConfig  `yaml:"catalog"`

	// 执行画像基线(app 层):model 取代原 default_model,reliability/loop/
	// digest/steps 为全体执行单元的降级源。
	Profile `yaml:",inline"`

	// Namespaces 是三层结构的主路径:tools(ns 内共享)→ components
	// (执行单元声明)→ skills(对外产品,唯一进目录的编排单元)。
	Namespaces []NamespaceConfig `yaml:"namespaces"`

	// Skills 是平铺声明的兼容路径,新配置建议用 namespaces。条目二选一:
	// 内部声明(prompt/engine/...)或外部引用(use: 链接,见 SkillEntry)。
	Skills []*SkillEntry `yaml:"skills"`
	Agents []AgentConfig `yaml:"agents"`

	// Skillpacks 是外部技能包策略(物化目录/获取策略/pin 政策)。
	Skillpacks SkillpacksConfig `yaml:"skillpacks"`
	// Models 是具名模型(skillpack frontmatter `model:` 按名引用)。
	Models []NamedModelConfig `yaml:"models"`
	Exec   ExecConfig         `yaml:"exec"`

	Serving  ServingConfig   `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`

	Suspend SuspendConfig `yaml:"suspend"`

	Observability ObservabilityConfig `yaml:"observability"`

	// StateDir 是可写运行状态目录,当前消费方是 skill 安装(skillpacks
	// 物化到 <state_dir>/agent-kit/.skills);file 后端与轨迹路径仍各自
	// 显式配置,尚未收口到这里。只读资源(配置/提示词/skill 包)不走
	// 这里——它们由资源 FS 承载(见 docs/resource-loading-design.md)。
	// 默认链:state_dir → 环境 AGENTKIT_STATE_DIR → $XDG_STATE_HOME/agentkit。
	StateDir string `yaml:"state_dir"`
	// WorkDirLegacy:work_dir 已拆义为只读根(资源 FS)+ 可写状态(state_dir),
	// 旧键装配期报错指路。
	WorkDirLegacy *string `yaml:"work_dir"`
	// DefaultModelLegacy:default_model 已改名 model(执行画像内嵌),
	// 旧键装配期报错指路。
	DefaultModelLegacy *ModelConfig `yaml:"default_model"`
}

// AppConfig 是应用级入口(app.yaml):进程级资源与接线板 + 全局默认。
// 执行画像(Profile)是所有 agent/component 的基线;会话状态与治理边界的
// app 层块为可选默认(agent 未声明整块时回落至此)。
type AppConfig struct {
	Secrets SecretsConfig `yaml:"secrets"`

	Prompts PromptsConfig `yaml:"prompts"`

	// Sources 是全局兼容源(直挂 agent 工具面的通用工具,如 fs)。
	Sources []SourceConfig `yaml:"sources"`
	Catalog CatalogConfig  `yaml:"catalog"`

	// 执行画像基线(app 层)。model 取代原 default_model。
	Profile `yaml:",inline"`

	// Stores 是 app 层具名存储实例(全局共享后端,如所有 agent 共用一个
	// redis session 实例);agent 层可再声明私有实例。
	Stores     []StoreInstance     `yaml:"stores"`
	Retrievers []RetrieverInstance `yaml:"retrievers"`

	// DefaultModelLegacy:default_model 已改名 model(执行画像内嵌),
	// 旧键装配期报错指路。
	DefaultModelLegacy *ModelConfig `yaml:"default_model"`

	// 会话状态 / 治理边界的 app 层默认(可选,agent 未声明整块时回落至此)。
	Session          SessionConfig         `yaml:"session"`
	Memory           MemoryConfig          `yaml:"memory"`
	Todo             TodoConfig            `yaml:"todo"`
	Approval         ApprovalConfig        `yaml:"approval"`
	Budget           BudgetConfig          `yaml:"budget"`
	StructuredOutput loop.StructuredConfig `yaml:"structured_output"`

	// Agents 是 agent 文件路径列表,相对 app.yaml 所在目录。
	Agents []string `yaml:"agents"`

	Serving  ServingConfig   `yaml:"serving"`
	Channels []ChannelConfig `yaml:"channels"`
	Suspend  SuspendConfig   `yaml:"suspend"`

	// Skillpacks 是外部技能包策略(全 app 一份,namespace 里的 use: 链接同样生效)。
	Skillpacks SkillpacksConfig `yaml:"skillpacks"`
	// Models 是具名模型(skillpack frontmatter `model:` 按名引用)。
	Models []NamedModelConfig `yaml:"models"`
	Exec   ExecConfig         `yaml:"exec"`

	Observability ObservabilityConfig `yaml:"observability"`

	// StateDir 同单文件 Config.StateDir:可写运行状态目录。
	StateDir string `yaml:"state_dir"`
	// WorkDirLegacy:旧键 work_dir 装配期报错指路 state_dir。
	WorkDirLegacy *string `yaml:"work_dir"`
}

// AgentFile 是 agent 维度的配置文件(agents/<name>.yaml)。
type AgentFile struct {
	AgentConfig `yaml:",inline"`
	// Namespaces 是关联的 namespace 挂载(相对本文件),自动挂载其全部
	// 导出 skill;capabilities.exclude 可屏蔽个别。每个挂载可携带 per-mount
	// 覆盖画像(最高优),兼容裸字符串写法(仅路径),见 NamespaceMount。
	Namespaces []NamespaceMount `yaml:"namespaces"`
}

// NamespaceFile 是 namespace 维度的配置文件(namespaces/<name>.yaml)。
// namespace 层可声明执行画像(内嵌自 NamespaceConfig 的 Profile,但不含
// model——能力不可自指模型)。
type NamespaceFile struct {
	NamespaceConfig `yaml:",inline"`
}

// ComponentConfig 声明一个执行单元:能力声明与能力使用分离的"声明"侧。
// 不进全局目录、对外不可见。执行画像(loop/reliability/digest/steps)内嵌自
// Profile(不含 model)。engine **必填**——执行形态决定成本模型与行为保证:
//
//	循环族(prompt + tools):engine = direct(单发:一次调用+一轮工具+
//	  收尾,无循环)| react(自主循环)| plan-execute(规划循环)| 已注册模板;
//	编排族(steps):engine = graph(DAG,可并行)| workflow(纯顺序,
//	  禁 needs)。两族字段互斥。
type ComponentConfig struct {
	Name   string `yaml:"name"`
	Engine string `yaml:"engine"`
	// Export 把该 component 导出给其他 namespace(经 imports 声明后以
	// cap://component/<ns>/<name> 全称引用)。默认私有;导出的 component
	// 不进目录、不可挂 agent 工具面——模型可选的能力必须走 skill。
	Export bool         `yaml:"export"`
	Prompt prompt.Value `yaml:"prompt"`
	// Tools 是循环族的工具面引用:tools/<source>/<name|*>(本 ns 工具)、
	// components/<name>(本 ns 执行单元)、cap://skill 引用(跨 ns)。
	Tools        []string       `yaml:"tools"`
	EngineConfig map[string]any `yaml:"engine_config"`

	// 执行画像 A 类(loop/reliability/digest/steps;不含 model)——component
	// 自己这一层,最近,压过 namespace/agent/app。
	Profile `yaml:",inline"`

	// Mode 是执行形态:subloop(缺省)| inline(过程卡,工具直挂宿主;
	// 语义见 skill.Declaration.Mode 与 docs/single-agent-mode-plan.md)。
	Mode string `yaml:"mode"`
	// Todo 给内部循环挂调用级临时清单(仅 react;调用结束即弃)。
	// 默认关——component 长到需要计划通常是"该拆成结构"的信号,
	// 这是给确实拆不动的研究型长循环的例外通道。
	Todo bool `yaml:"todo"`

	// 编排族:私有的无脑序列/图(字段语义同 skill 的对应项)。
	Params map[string]capability.ParamDecl `yaml:"params"`
	Steps  []engine.Step                   `yaml:"steps"`
	Output string                          `yaml:"output"`
	// Deliver 是产出的交付语义(attach|always|direct,缺省=证据)。
	Deliver string `yaml:"deliver"`
}

// NamespaceSkill 声明一个对外 skill:接口(描述+参数)+ 编排(steps,
// 纯引用)。steps 的语义是 DAG,见 engine.Step。
type NamespaceSkill struct {
	Name        string                          `yaml:"name"`
	Version     string                          `yaml:"version"`
	Description string                          `yaml:"description"`
	Params      map[string]capability.ParamDecl `yaml:"params"`
	Steps       []engine.Step                   `yaml:"steps"`
	Output      string                          `yaml:"output"`
	// Engine 是编排形态:graph(DAG,可并行,缺省)| workflow(严格
	// 顺序,禁 needs)。与 component 的同名字段同一词汇;skill 允许
	// 缺省(graph)是因为 skill 只有编排一族,不存在循环/编排歧义。
	Engine string `yaml:"engine"`
	// Deliver 是产出的交付语义(attach|always|direct,缺省=证据),
	// 词汇同 skill.Declaration,装配期枚举校验。
	Deliver string `yaml:"deliver"`
	// From 集成一个外部 SKILL.md 技能包(github.com/...@ver |
	// https://...zip | file:...);须显式 name,integrity/tools/context
	// 见 SkillEntry 同名字段。与 steps/use 互斥。
	From string `yaml:"from"`
	// MaxRounds 是 from 技能包内部循环的轮数覆盖(与平铺 SkillEntry 的
	// max_rounds 同义;两条 skillpack 装配路径同一配置面)。
	MaxRounds int `yaml:"max_rounds"`
	// Use 是入口引用形态(与 steps 互斥):components/<name> 等引用,
	// skill 退化为纯接口声明,执行整体委托给该能力,params JSON 原样
	// 透传。外部链接已改用 from(写在这里装配期报错指路)。
	Use       string   `yaml:"use"`
	Integrity string   `yaml:"integrity"`
	Tools     []string `yaml:"tools"`
	Context   string   `yaml:"context"`
	// StepDefaults 是本 skill 步骤未声明 timeout/retry 时的缺省
	// (override 链的 skill 层;更下层的步骤显式声明优先)。
	StepDefaults struct {
		Timeout loop.Duration `yaml:"timeout"`
		Retry   int           `yaml:"retry"`
	} `yaml:"step_defaults"`
}

// NamespaceConfig 是一个配置命名空间的完整声明。执行画像(loop/reliability/
// digest/steps;不含 model)内嵌自 Profile——该 ns 下 component 的画像默认,
// 覆盖 agent、被 component 覆盖。
type NamespaceConfig struct {
	Name    string `yaml:"name"`
	Profile `yaml:",inline"`
	// Sources 声明能力供给源(与顶层 sources: 同构);"声明源用 sources、
	// 引用工具面用 tools"全库一致。
	Sources []SourceConfig `yaml:"sources"`
	// Tools 已废弃:源声明改用 sources(装配期报错指路)。
	ToolsLegacy []SourceConfig `yaml:"tools"`
	// Imports 声明依赖的 namespace(其导出 component 经
	// cap://component/<ns>/<name> 可见)。可见性按装配/挂载顺序:被
	// 依赖的 ns 必须先声明/先挂载,否则装配期报错——与 cap://skill
	// 的"按关联顺序可见"同一规则。
	Imports    []string          `yaml:"imports"`
	Components []ComponentConfig `yaml:"components"`
	Skills     []NamespaceSkill  `yaml:"skills"`
}

// SkillEntry 是 skills: 列表的一个条目:内部声明(内嵌 skill.Declaration)
// 或外部引用(use: 链接)二选一。外部形态把市面 SKILL.md 技能包一行集成
// 进来(装配期物化到 .skills,见 skillpack.go),其余字段作本地覆盖:
// name 覆盖 ns/名字,model/max_steps 沿用内嵌声明的同名字段,tools 在
// 包的 allowed-tools 之上再收紧(交集),context: fork 以调用方对话快照
// 起步(与编排步骤同义)。
type SkillEntry struct {
	skill.Declaration `yaml:",inline"`
	From              string   `yaml:"from"`      // 外部获取来源:github.com/...@ver | https://...zip | file:...
	Use               string   `yaml:"use"`       // 已废弃:外链改用 from(装配期报错指路)
	Integrity         string   `yaml:"integrity"` // sha256:<hex>,可选强校验
	Tools             []string `yaml:"tools"`     // 白名单收紧(∩ allowed-tools)
	Context           string   `yaml:"context"`   // fresh(默认)| fork
}
