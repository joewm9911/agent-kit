// profile.go 定义统一执行画像(execution profile)与其分层降级合并。
//
// 一套 schema、四层可声明:任何执行单元(主 loop 或 component)共用同一组
// 执行参数(model/loop/reliability/digest/steps),声明在哪层就在哪层生效,
// 缺失则沿 app → agent → namespace → component 逐级向上降级(就近者胜),
// 外加 agent 给某 namespace 的 per-mount 指定(最高优)。字段全部可判空
// (指针/空串):nil=继承、非 nil=本层生效——才能区分"没配"与"配成零值"。
//
// model 是特例:能力(namespace/component)不能自己指定 model(部署/成本
// 决策由集成方定),故 namespace/component 的 Profile.Model 必须为 nil,
// 由 validateNoModel 在装配期强制;可声明 model 的只有 app / agent自己 /
// per-mount。因 ns/component 贡献 nil,通用 merge 天然得到 model 的三级链。
package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/runtime/loop"
)

// Profile 是执行画像:app/agent/namespace/component 各内嵌一份(YAML inline),
// per-mount 覆盖亦是同形 Profile,五级就近合并得 component 生效值。
type Profile struct {
	Model       *ModelConfig       `yaml:"model"`
	Loop        LoopProfile        `yaml:"loop"`
	Reliability ReliabilityProfile `yaml:"reliability"`
	Digest      DigestProfile      `yaml:"digest"`
	// StepDefaultsLegacy 已随编排族移除(固定流程下沉宿主 eino compose),
	// 误写装配期报错指路。
	StepDefaultsLegacy map[string]any `yaml:"step_defaults"`
}

// LoopProfile 是执行单元的循环控制:迭代上限 + 上下文压缩。compaction 归
// loop(而非 session)——它压缩的是"执行单元循环的工作上下文",主 loop 与
// component 都有,只有主 loop 额外有 session;归 loop 才能全链降级到 component。
type LoopProfile struct {
	// MaxSteps 是工具调用的轮数上限(一轮 = 一次模型决策 + 一批工具执行;
	// react 装配时换算为 eino 的节点步数 2N+1,额外的 1 是收尾作答)。
	// 默认 12 轮。yaml 键 max_rounds——名实对齐(内部字段名沿用)。
	MaxSteps *int `yaml:"max_rounds"`
	// MaxStepsLegacy 已废弃:max_steps 语义即轮数,改名 max_rounds(报错指路)。
	MaxStepsLegacy *int                   `yaml:"max_steps"`
	Compaction     *loop.CompactionConfig `yaml:"compaction"`
}

// ReliabilityProfile 是执行单元的可靠性:工具单次调用超时 + 模型瞬时错误重试。
type ReliabilityProfile struct {
	ToolTimeout *loop.Duration    `yaml:"tool_timeout"`
	Retry       *loop.RetryConfig `yaml:"retry"`
}

// DigestProfile 是"大工具结果进上下文前的处理":over 触发消化、truncate 硬
// 截断兜底、store 暂存后端(供 read_result 取回)。
type DigestProfile struct {
	Over     *int `yaml:"over"`
	Truncate *int `yaml:"truncate"`
	// DegradeKeep 是暂存后端不可用时的应急保留量(rune,缺省 24000):
	// 指针发不出去还只留 truncate 的量 = 不必要的数据损失;完全不截又会
	// 炸上下文窗(生产实测单结果 10 万字符)。中小结果在降级态零损失,
	// 极端长文仍有物理护栏。
	DegradeKeep *int           `yaml:"degrade_keep"`
	Store       string         `yaml:"store"`
	StoreConfig map[string]any `yaml:"store_config"`
}

// merge 返回合并结果:nearer(更近/更高优层级)显式设置的键覆盖 p,其余
// 继续沿用 p。逐字段(per-field)降级——某层只声明块里的一个字段,不影响
// 同块其它字段继续向上取值。策略叶子(model/compaction/retry)整体 set-或-
// 继承,不跨层拼半个策略。
func (p Profile) merge(nearer Profile) Profile {
	out := p
	if nearer.Model != nil {
		out.Model = nearer.Model
	}
	if nearer.Loop.MaxSteps != nil {
		out.Loop.MaxSteps = nearer.Loop.MaxSteps
	}
	if nearer.Loop.Compaction != nil {
		out.Loop.Compaction = nearer.Loop.Compaction
	}
	if nearer.Reliability.ToolTimeout != nil {
		out.Reliability.ToolTimeout = nearer.Reliability.ToolTimeout
	}
	if nearer.Reliability.Retry != nil {
		out.Reliability.Retry = nearer.Reliability.Retry
	}
	if nearer.Digest.Over != nil {
		out.Digest.Over = nearer.Digest.Over
	}
	if nearer.Digest.Truncate != nil {
		out.Digest.Truncate = nearer.Digest.Truncate
	}
	if nearer.Digest.DegradeKeep != nil {
		out.Digest.DegradeKeep = nearer.Digest.DegradeKeep
	}
	if nearer.Digest.Store != "" {
		out.Digest.Store = nearer.Digest.Store
		out.Digest.StoreConfig = nearer.Digest.StoreConfig
	}
	return out
}

// rejectLegacyKeys 拦截已改名/已移除的旧配置键(fail fast 即迁移指南)。
func (p Profile) rejectLegacyKeys(where string) error {
	if p.Loop.MaxStepsLegacy != nil {
		return fmt.Errorf("%s: max_steps has been renamed max_rounds (the semantics were always rounds: one round = one model decision + one batch of tools)", where)
	}
	if p.StepDefaultsLegacy != nil {
		return fmt.Errorf("%s: step_defaults has been removed along with the orchestration family — fixed flows live in host code via eino compose (see examples/pipeline)", where)
	}
	return nil
}

// validateNoModel 强制"能力不可自指 model":namespace/component 的 Profile
// 不得声明 model。装配期调用,fail fast。
func (p Profile) validateNoModel(where string) error {
	if p.Model != nil {
		return fmt.Errorf("%s: model cannot be declared here (a capability cannot pick its own model; model is configured in only three places: app / the agent itself / the agent giving one to a namespace)", where)
	}
	return nil
}

// ---- 解析出的具体值(nil → 框架内置默认的零值,由下游各自兜底)----

func (p Profile) maxSteps() int {
	if p.Loop.MaxSteps != nil {
		return *p.Loop.MaxSteps
	}
	return 0
}

func (p Profile) compaction() loop.CompactionConfig {
	if p.Loop.Compaction != nil {
		return *p.Loop.Compaction
	}
	return loop.CompactionConfig{}
}

func (p Profile) toolTimeout() loop.Duration {
	if p.Reliability.ToolTimeout != nil {
		return *p.Reliability.ToolTimeout
	}
	return 0
}

func (p Profile) retry() loop.RetryConfig {
	if p.Reliability.Retry != nil {
		return *p.Reliability.Retry
	}
	return loop.RetryConfig{}
}

func (p Profile) digestOver() int {
	if p.Digest.Over != nil {
		return *p.Digest.Over
	}
	return 0
}

func (p Profile) digestTruncate() int {
	if p.Digest.Truncate != nil {
		return *p.Digest.Truncate
	}
	return 0
}

func (p Profile) degradeKeep() int {
	if p.Digest.DegradeKeep != nil {
		return *p.Digest.DegradeKeep
	}
	return 0 // 0 = 用 loop 内置默认(24000)
}


// NamespaceMount 是 agent 对一个 namespace 的挂载:路径 + per-mount 覆盖画像。
// 覆盖画像是五级链里的最高优,且只装执行画像 A 类(component 实际拥有的
// 配置);model 可在此指定(集成方给该 mount 显式选模型)。
//
// YAML 兼容两种写法:裸字符串(仅路径,无覆盖)或映射(path + 覆盖字段):
//
//	namespaces:
//	  - ../namespaces/catalog.yaml                     # 仅路径
//	  - path: ../namespaces/research.yaml              # 路径 + 覆盖
//	    model: {provider: openai, config: {...}}
//	    loop:  {max_rounds: 5}
type NamespaceMount struct {
	Path    string `yaml:"path"`
	Profile `yaml:",inline"`
}

// UnmarshalYAML 接受裸字符串(路径)或映射(路径 + 覆盖画像)。
func (m *NamespaceMount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&m.Path)
	}
	type raw NamespaceMount
	return node.Decode((*raw)(m))
}
