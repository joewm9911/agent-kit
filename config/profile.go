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

	"github.com/joewm9911/agent-kit/loop"
)

// Profile 是执行画像:app/agent/namespace/component 各内嵌一份(YAML inline),
// per-mount 覆盖亦是同形 Profile,五级就近合并得 component 生效值。
type Profile struct {
	Model       *ModelConfig       `yaml:"model"`
	Loop        LoopProfile        `yaml:"loop"`
	Reliability ReliabilityProfile `yaml:"reliability"`
	Digest      DigestProfile      `yaml:"digest"`
	// StepDefaults 是编排步骤的 timeout/retry 缺省。键名 step_defaults 而非
	// steps——后者是编排族 component 的步骤列表(结构声明),两者语义不同;
	// 与 skill 层的 step_defaults 词汇一致。
	StepDefaults StepDefaultsProfile `yaml:"step_defaults"`
}

// LoopProfile 是执行单元的循环控制:迭代上限 + 上下文压缩。compaction 归
// loop(而非 session)——它压缩的是"执行单元循环的工作上下文",主 loop 与
// component 都有,只有主 loop 额外有 session;归 loop 才能全链降级到 component。
type LoopProfile struct {
	MaxSteps   *int                   `yaml:"max_steps"`
	Compaction *loop.CompactionConfig `yaml:"compaction"`
}

// ReliabilityProfile 是执行单元的可靠性:工具单次调用超时 + 模型瞬时错误重试。
type ReliabilityProfile struct {
	ToolTimeout *loop.Duration    `yaml:"tool_timeout"`
	Retry       *loop.RetryConfig `yaml:"retry"`
}

// DigestProfile 是"大工具结果进上下文前的处理":over 触发消化、truncate 硬
// 截断兜底、store 暂存后端(供 read_result 取回)。
type DigestProfile struct {
	Over        *int           `yaml:"over"`
	Truncate    *int           `yaml:"truncate"`
	Store       string         `yaml:"store"`
	StoreConfig map[string]any `yaml:"store_config"`
}

// StepDefaultsProfile 是编排步骤(有 steps 的执行单元)未声明 timeout/retry
// 时的缺省。
type StepDefaultsProfile struct {
	Timeout *loop.Duration `yaml:"timeout"`
	Retry   *int           `yaml:"retry"`
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
	if nearer.Digest.Store != "" {
		out.Digest.Store = nearer.Digest.Store
		out.Digest.StoreConfig = nearer.Digest.StoreConfig
	}
	if nearer.StepDefaults.Timeout != nil {
		out.StepDefaults.Timeout = nearer.StepDefaults.Timeout
	}
	if nearer.StepDefaults.Retry != nil {
		out.StepDefaults.Retry = nearer.StepDefaults.Retry
	}
	return out
}

// validateNoModel 强制"能力不可自指 model":namespace/component 的 Profile
// 不得声明 model。装配期调用,fail fast。
func (p Profile) validateNoModel(where string) error {
	if p.Model != nil {
		return fmt.Errorf("%s: model 不能在此声明(能力不能自己指定模型;model 只由 app / agent自己 / agent给namespace指定 三处配置)", where)
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

func (p Profile) stepTimeout() loop.Duration {
	if p.StepDefaults.Timeout != nil {
		return *p.StepDefaults.Timeout
	}
	return 0
}

func (p Profile) stepRetry() int {
	if p.StepDefaults.Retry != nil {
		return *p.StepDefaults.Retry
	}
	return 0
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
//	    loop:  {max_steps: 5}
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
