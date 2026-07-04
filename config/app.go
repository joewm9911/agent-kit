// app.go 实现配置的多文件形态:按所有权切分为三层文件——
//
//	app.yaml               应用级(部署拥有):进程级资源 + 接线板,业务含量为零
//	agents/<name>.yaml     agent 维度(产品面拥有):模型/记忆/预算/审批 + namespace 关联
//	namespaces/<name>.yaml namespace 维度(域团队拥有):tools/components/skills
//
// 约定:文件名即名字(显式 name 必须一致);相对路径相对引用它的文件
// 解析;agent 关联 namespace 即自动挂载其全部导出 skill。
//
// 装配语义:namespace 是库,agent 挂载时按自己的默认值实例化一份
// (源连接按 namespace 文件缓存共享,components/skills 装配按 agent
// 实例化);跨 namespace 的 cap://skill 引用在同一 agent 挂载的集合内
// 按关联顺序解析。
//
// override 链(执行参数,就近优先,显式写了的键才生效):
//
//	component/step → skill(step_defaults) → namespace(defaults) → agent(defaults) → app
//
// 治理策略(approval/budget/max_risk)不进链——那是 agent/部署持有的
// 安全边界,库不能给自己放权。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/channel"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/observe"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/registry"
	"github.com/joewm9911/agent-kit/serving"
	"github.com/joewm9911/agent-kit/source"
	"github.com/joewm9911/agent-kit/suspend"
)

// merge 返回合并结果:nearer(更近层级)显式设置的键覆盖 d。
func (d Defaults) merge(nearer Defaults) Defaults {
	out := d
	if nearer.Model != nil {
		out.Model = nearer.Model
	}
	if nearer.MaxSteps != nil {
		out.MaxSteps = nearer.MaxSteps
	}
	if nearer.Compaction != nil {
		out.Compaction = nearer.Compaction
	}
	if nearer.ToolTimeout != nil {
		out.ToolTimeout = nearer.ToolTimeout
	}
	if nearer.Retry != nil {
		out.Retry = nearer.Retry
	}
	if nearer.StepTimeout != nil {
		out.StepTimeout = nearer.StepTimeout
	}
	if nearer.StepRetry != nil {
		out.StepRetry = nearer.StepRetry
	}
	if nearer.DigestOver != nil {
		out.DigestOver = nearer.DigestOver
	}
	return out
}

// AgentSpec 是解析后的 agent 文件及其关联的 namespace 文件(保序)。
type AgentSpec struct {
	AgentFile
	Path   string
	Mounts []*NamespaceSpec
}

// NamespaceSpec 是解析后的 namespace 文件。
type NamespaceSpec struct {
	NamespaceFile
	Path string // 绝对路径,亦作源连接缓存键
}

// AppSpec 是 LoadApp 的产物:全部文件解析完毕、名字与路径校验通过。
type AppSpec struct {
	App    AppConfig
	Agents []*AgentSpec
}

// LoadApp 读取多文件形态的应用声明。secrets provider 由 app.yaml 声明,
// 对全部文件的 ${ENV}/${secret:NAME} 占位符统一生效。
func LoadApp(path string) (*AppSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sp, err := secretsProviderFor(raw, path)
	if err != nil {
		return nil, err
	}
	var app AppConfig
	if err := expandParse(raw, sp, path, &app); err != nil {
		return nil, err
	}

	appDir := filepath.Dir(path)
	spec := &AppSpec{App: app}
	nsCache := map[string]*NamespaceSpec{} // 绝对路径 → 解析结果(多 agent 共享解析)
	seen := map[string]string{}            // agent 名 → 文件,重名报错

	for _, rel := range app.Agents {
		agentPath := filepath.Join(appDir, rel)
		araw, err := os.ReadFile(agentPath)
		if err != nil {
			return nil, fmt.Errorf("agent file %s: %w", rel, err)
		}
		var af AgentFile
		if err := expandParse(araw, sp, agentPath, &af); err != nil {
			return nil, err
		}
		if err := applyFileName(&af.Name, agentPath, "agent"); err != nil {
			return nil, err
		}
		if prev, dup := seen[af.Name]; dup {
			return nil, fmt.Errorf("agent %q declared by both %s and %s", af.Name, prev, agentPath)
		}
		seen[af.Name] = agentPath

		as := &AgentSpec{AgentFile: af, Path: agentPath}
		agentDir := filepath.Dir(agentPath)
		for _, nsRel := range af.Namespaces {
			nsPath, err := filepath.Abs(filepath.Join(agentDir, nsRel))
			if err != nil {
				return nil, err
			}
			ns, ok := nsCache[nsPath]
			if !ok {
				nraw, err := os.ReadFile(nsPath)
				if err != nil {
					return nil, fmt.Errorf("agent %s: namespace file %s: %w", af.Name, nsRel, err)
				}
				var nf NamespaceFile
				if err := expandParse(nraw, sp, nsPath, &nf); err != nil {
					return nil, err
				}
				if err := applyFileName(&nf.Name, nsPath, "namespace"); err != nil {
					return nil, err
				}
				ns = &NamespaceSpec{NamespaceFile: nf, Path: nsPath}
				nsCache[nsPath] = ns
			}
			as.Mounts = append(as.Mounts, ns)
		}
		spec.Agents = append(spec.Agents, as)
	}
	return spec, nil
}

// applyFileName 落实"文件名即名字":name 为空取文件名,显式声明则必须一致。
func applyFileName(name *string, path, kind string) error {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if *name == "" {
		*name = base
		return nil
	}
	if *name != base {
		return fmt.Errorf("%s file %s: declared name %q does not match file name %q", kind, path, *name, base)
	}
	return nil
}

// BuildApp 把多文件声明组装为可运行的 App。与单文件 Build 的关键差异:
// namespace 按 agent 实例化(执行参数 override 链得以生效),源连接按
// namespace 文件缓存共享,agent 工具面 = 关联 namespace 的全部导出
// skill + 全局兼容源的 include 选品。
func BuildApp(ctx context.Context, spec *AppSpec, opts BuildOptions) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ac := &spec.App

	// 1. 可观测性
	if ac.Observability.Log {
		observe.Install(logger)
	}
	if p := ac.Observability.TrajectoryPath; p != "" {
		h, err := observe.Trajectory(p)
		if err != nil {
			return nil, err
		}
		callbacks.AppendGlobalHandlers(h)
	}

	// 2. 提示词
	var prompts *prompt.Resolver
	if len(ac.Prompts.Sources) > 0 {
		prompts = prompt.NewResolver(ac.Prompts.DefaultLabel)
		for _, ps := range ac.Prompts.Sources {
			p, err := prompt.NewProvider(ps.Type, ps.Config)
			if err != nil {
				return nil, fmt.Errorf("prompt source %s: %w", ps.Name, err)
			}
			prompts.Add(ps.Name, p)
		}
	}

	// 3. 全局目录:兼容源 + 代码侧能力(agent include 选品的来源)
	maxRisk := capability.RiskMutating
	if ac.Catalog.MaxRisk != "" {
		var err error
		if maxRisk, err = capability.ParseRisk(ac.Catalog.MaxRisk); err != nil {
			return nil, err
		}
	}
	global := source.NewCatalog(maxRisk, logger)
	for _, sc := range ac.Sources {
		src, err := source.New(ctx, sc.Type, sc.Name, sc.Config)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", sc.Name, err)
		}
		if err := global.AddSource(ctx, src, sc.Required, sc.Priority); err != nil {
			return nil, err
		}
	}
	if len(opts.ExtraCapabilities) > 0 {
		if err := global.Add(opts.ExtraCapabilities...); err != nil {
			return nil, err
		}
	}

	// 4. app 默认模型(Ring 0 包装同单文件路径)
	var defaultModel model.ToolCallingChatModel
	if ac.DefaultModel != nil {
		m, err := registry.BuildModel(ctx, ac.DefaultModel.Provider, ac.DefaultModel.Config)
		if err != nil {
			return nil, fmt.Errorf("default_model: %w", err)
		}
		defaultModel = loop.BudgetModel(loop.RetryModel(m, ac.Reliability.ModelRetry))
	}

	// 5. agents:每个 agent 实例化自己关联的 namespaces
	app := &App{
		Agents: map[string]*agent.Agent{}, Catalog: global, Prompts: prompts,
		AgentMounts: map[string]*source.Catalog{},
	}
	srcCache := newSourceCache()
	for _, as := range spec.Agents {
		a, mounted, err := buildAgentFromSpec(ctx, as, global, prompts, defaultModel, ac.Reliability, maxRisk, srcCache, opts)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", as.Name, err)
		}
		app.Agents[a.Name()] = a
		app.AgentMounts[a.Name()] = mounted
	}

	// 6. gateway 与 IM 通道(接线板)
	if ac.Serving.Addr != "" {
		agents := make([]*agent.Agent, 0, len(app.Agents))
		for _, a := range app.Agents {
			agents = append(agents, a)
		}
		app.Server = serving.New(ac.Serving.Addr, agents, logger)
		dispatcher := channel.NewDispatcher(logger)
		if ac.Suspend.Dir != "" {
			store, err := suspend.NewFileStore(ac.Suspend.Dir)
			if err != nil {
				return nil, fmt.Errorf("suspend: %w", err)
			}
			dispatcher.EnableSuspend(store)
		}
		for _, cc := range ac.Channels {
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
	} else if len(ac.Channels) > 0 {
		return nil, fmt.Errorf("channels configured but serving.addr is empty")
	}

	return app, nil
}

// buildAgentFromSpec 装配一个 agent:按关联顺序实例化 namespaces
// (跨 ns 引用在本 agent 的挂载集合内解析)→ 自动挂载全部导出 skill
// → 叠加全局兼容源的 include 选品 → 交给 buildAgent。
func buildAgentFromSpec(ctx context.Context, as *AgentSpec, global *source.Catalog,
	prompts *prompt.Resolver, defaultModel model.ToolCallingChatModel,
	rel ReliabilityConfig, maxRisk capability.Risk, srcCache *sourceCache,
	opts BuildOptions) (*agent.Agent, *source.Catalog, error) {

	// agent 的挂载目录:本 agent 关联的 namespaces 导出的 skill 落在
	// 这里,同时充当跨 ns cap://skill 引用的解析域(按关联顺序可见)。
	mounted := source.NewCatalog(maxRisk, nil)
	for _, ns := range as.Mounts {
		nsCopy := ns.NamespaceConfig // 按 agent 实例化,不共享装配产物
		err := buildNamespace(ctx, &nsCopy, nsDeps{
			global: mounted, prompts: prompts, defaultModel: defaultModel,
			maxRisk: maxRisk, toolTimeout: rel.ToolTimeout, retry: rel.ModelRetry,
			defaults: as.Defaults.merge(ns.Defaults), // ns 更近,覆盖 agent 默认
			nsPath:   ns.Path, srcCache: srcCache,
		})
		if err != nil {
			return nil, nil, err
		}
	}

	// 自动挂载:关联 namespaces 的全部导出 skill(exclude 可屏蔽)
	caps, err := mounted.SelectAll(as.Capabilities.Exclude)
	if err != nil {
		return nil, nil, err
	}
	// 兼容路径:全局源的 include 选品(直挂工具等)
	if len(as.Capabilities.Include) > 0 {
		picked, err := global.Select(as.Capabilities.Include, as.Capabilities.Exclude)
		if err != nil {
			return nil, nil, err
		}
		caps = append(caps, picked...)
	}

	m, err := agentModel(ctx, as.Model, as.Defaults.Model, defaultModel, rel)
	if err != nil {
		return nil, nil, err
	}
	a, err := buildAgent(ctx, &as.AgentConfig, caps, prompts, m, rel, opts.Interactor, nil)
	if err != nil {
		return nil, nil, err
	}
	return a, mounted, nil
}
