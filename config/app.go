// app.go 实现配置的多文件形态:按所有权切分为三层文件——
//
//	app.yaml               应用级(部署拥有):进程级资源 + 接线板,业务含量为零
//	agents/<name>.yaml     agent 维度(产品面拥有):模型/记忆/预算/审批 + namespace 关联
//	namespaces/<name>.yaml namespace 维度(域团队拥有):tools/components/skills
//
// 约定:文件名即名字(显式 name 必须一致);相对路径相对引用它的文件
// 解析;agent 关联 namespace 即自动挂载其全部导出 skill。
//
// 装配语义:namespace 是库,agent 挂载时按解析出的执行画像实例化一份
// (源连接按 namespace 文件缓存共享,components/skills 装配按 agent
// 实例化);跨 namespace 的 cap://skill 引用在同一 agent 挂载的集合内
// 按关联顺序解析。
//
// 执行画像 A 类(model/loop/reliability/digest/step_defaults)五级就近降级
// (高→低,见 profile.go):
//
//	agent给该ns指定(per-mount) → component → namespace → agent自己 → app
//
// model 特例:能力不可自指,ns/component 不参与,链退化为 per-mount →
// agent自己 → app。会话状态(session/memory/todo)与治理边界(approval/
// budget/structured_output)是 B/C 类:app→agent 整块降级,不下沉 component
// ——那是 agent/部署持有的安全边界,库不能给自己放权。
package config

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/resource"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/serving"
)

// inheritAppDefaults 把 app 层的会话状态 / 治理边界默认下沉到 agent:
// agent 未声明整块时回落 app(B/C 类是 app→agent 整块降级,不下沉 component)。
// 具名存储实例:app 层作为共享池,agent 私有实例前置(名字冲突 agent 优先)。
func inheritAppDefaults(app *AppConfig, ac *AgentConfig) {
	if ac.Session.isZero() {
		ac.Session = app.Session
	}
	if ac.Memory.isZero() {
		ac.Memory = app.Memory
	}
	if ac.Todo.isZero() {
		ac.Todo = app.Todo
	}
	if ac.Approval.isZero() {
		ac.Approval = app.Approval
	}
	if ac.Budget.isZero() {
		ac.Budget = app.Budget
	}
	if ac.StructuredOutput == (loop.StructuredConfig{}) {
		ac.StructuredOutput = app.StructuredOutput
	}
	if len(app.Stores) > 0 {
		ac.Stores = append(append([]StoreInstance{}, ac.Stores...), app.Stores...)
	}
	if len(app.Retrievers) > 0 {
		ac.Retrievers = append(append([]RetrieverInstance{}, ac.Retrievers...), app.Retrievers...)
	}
}

// Mount 是 agent 对一个 namespace 的解析后挂载:namespace 文件 + per-mount
// 覆盖画像(五级链的最高优)。嵌入 *NamespaceSpec,故 .Name/.Path/
// .NamespaceConfig 直接可用。
type Mount struct {
	*NamespaceSpec
	Override Profile
}

// AgentSpec 是解析后的 agent 文件及其关联的 namespace 挂载(保序)。
type AgentSpec struct {
	AgentFile
	Path   string
	Mounts []Mount
}

// NamespaceSpec 是解析后的 namespace 文件。
type NamespaceSpec struct {
	NamespaceFile
	Path string // 资源 FS 内路径('/' 分隔),亦作源连接缓存键
}

// AppSpec 是 LoadApp 的产物:全部文件解析完毕、名字与路径校验通过。
// Root 是加载它的只读资源 FS(prompt/skill 等只读子资源从此解析,与配置
// 同源;见 docs/resource-loading-design.md)。
type AppSpec struct {
	App    AppConfig
	Agents []*AgentSpec
	Root   fs.FS
}

// LoadApp 读取多文件形态的应用声明。secrets provider 由 app.yaml 声明,
// 对全部文件的 ${ENV}/${secret:NAME} 占位符统一生效。
func LoadApp(ref string) (*AppSpec, error) {
	root, entry, err := resource.Resolve(ref)
	if err != nil {
		return nil, err
	}
	return LoadAppFS(root, entry)
}

// LoadAppFS 从只读资源 FS 加载多文件声明。entry 是 FS 内的入口路径
// (fs.FS 语义:'/' 分隔、无 '..' 逃逸);agent/namespace 的相对引用一律
// 在这个 FS 内解析,唯一锚点是 FS 根,不依赖进程 CWD。本地盘走
// LoadApp(path);内嵌走 LoadAppFS(embed.FS, "config/app.yaml")。
func LoadAppFS(root fs.FS, entry string) (*AppSpec, error) {
	raw, err := fs.ReadFile(root, entry)
	if err != nil {
		return nil, fmt.Errorf("app file %s: %w", entry, err)
	}
	sp, err := secretsProviderFor(raw, entry)
	if err != nil {
		return nil, err
	}
	var app AppConfig
	if err := expandParse(raw, sp, entry, &app); err != nil {
		return nil, err
	}
	appDir := path.Dir(entry)
	spec := &AppSpec{App: app, Root: root}
	nsCache := map[string]*NamespaceSpec{} // FS 内路径 → 解析结果(多 agent 共享解析)
	seen := map[string]string{}            // agent 名 → 文件,重名报错

	for _, rel := range app.Agents {
		agentPath := path.Join(appDir, rel)
		araw, err := fs.ReadFile(root, agentPath)
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
		agentDir := path.Dir(agentPath)
		for _, mnt := range af.Namespaces {
			if mnt.Path == "" {
				return nil, fmt.Errorf("agent %s: namespace mount missing path", af.Name)
			}
			nsPath := path.Join(agentDir, mnt.Path) // FS 内路径,不逃出根
			// per-mount 覆盖是集成方(agent)对该 namespace 的显式指定,
			// 属执行画像 A 类;若误写 session/approval 等非 A 类字段,
			// yaml inline 会静默忽略——这里不额外校验(A 类字段之外无处安放)。
			ns, ok := nsCache[nsPath]
			if !ok {
				nraw, err := fs.ReadFile(root, nsPath)
				if err != nil {
					return nil, fmt.Errorf("agent %s: namespace file %s: %w", af.Name, mnt.Path, err)
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
			as.Mounts = append(as.Mounts, Mount{NamespaceSpec: ns, Override: mnt.Profile})
		}
		spec.Agents = append(spec.Agents, as)
	}
	return spec, nil
}

// buildPromptProvider 构造一个 prompt provider。type: file 时锚到资源 FS
// (与配置同源:prompt 目录相对配置根解析,不依赖进程 CWD);其余类型
// (inline/http/自定义)走通用注册表。root 为 nil(单文件 Build 无资源 FS)
// 时 file 回落注册表工厂(os.DirFS,相对 CWD)。
func buildPromptProvider(ps PromptSourceConfig, root fs.FS) (prompt.Provider, error) {
	if ps.Type == "file" && root != nil {
		dir, _ := ps.Config["dir"].(string)
		if dir == "" {
			return nil, fmt.Errorf("file prompt source: dir is required")
		}
		sub, err := fs.Sub(root, path.Clean(dir))
		if err != nil {
			return nil, err
		}
		return prompt.NewFileProvider(sub), nil
	}
	return prompt.NewProvider(ps.Type, ps.Config)
}

// applyFileName 落实"文件名即名字":name 为空取文件名,显式声明则必须一致。
func applyFileName(name *string, fsPath, kind string) error {
	base := path.Base(fsPath)
	base = strings.TrimSuffix(base, path.Ext(base))
	if *name == "" {
		*name = base
		return nil
	}
	if *name != base {
		return fmt.Errorf("%s file %s: declared name %q does not match file name %q", kind, fsPath, *name, base)
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

	// 1. 可观测性(进程级幂等账本在 observe.go)
	if err := installObservability(ac.Observability, logger); err != nil {
		return nil, err
	}
	if err := ac.Profile.rejectLegacyKeys("app.yaml"); err != nil {
		return nil, err
	}
	if err := rejectWorkDir(ac.WorkDirLegacy, "app.yaml"); err != nil {
		return nil, err
	}
	if ac.DefaultModelLegacy != nil {
		return nil, fmt.Errorf("app.yaml: default_model has been renamed model (part of the execution profile; same provider/config shape)")
	}

	// 2. 提示词
	var prompts *prompt.Resolver
	if len(ac.Prompts.Sources) > 0 {
		prompts = prompt.NewResolver(ac.Prompts.DefaultLabel)
		for _, ps := range ac.Prompts.Sources {
			p, err := buildPromptProvider(ps, spec.Root)
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
		sconf := sc.Config
		if sc.Type == "exec" {
			sconf = ac.Exec.injectInto(sconf)
		}
		src, err := source.New(ctx, sc.Type, sc.Name, sconf)
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
	var defaultModel einomodel.ToolCallingChatModel
	if ac.Profile.Model != nil {
		m, err := model.Build(ctx, ac.Profile.Model.Provider, ac.Profile.Model.Config)
		if err != nil {
			return nil, fmt.Errorf("model: %w", err)
		}
		defaultModel = loop.BudgetModel(loop.RetryModel(m, ac.Profile.retry())) // 质量守卫在循环装配层(ReviewModel)
	}

	// 5. agents:每个 agent 实例化自己关联的 namespaces
	app := &App{
		Agents: map[string]*agent.Agent{}, Catalog: global, Prompts: prompts,
		AgentMounts: map[string]*source.Catalog{},
	}
	srcCache := newSourceCache()
	// 具名解析环境:agent 注册表(agents 建成后回填)+ 具名模型 Hub。
	agentNames := make([]string, 0, len(spec.Agents))
	for _, as := range spec.Agents {
		agentNames = append(agentNames, as.Name)
	}
	hubs, err := newSkillHubs(ac.Models, ac.Profile.retry(), agentNames)
	if err != nil {
		return nil, err
	}
	for _, as := range spec.Agents {
		a, mounted, err := buildAgentFromSpec(ctx, as, ac, global, prompts, defaultModel, maxRisk, srcCache, hubs, opts)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", as.Name, err)
		}
		app.Agents[a.Name()] = a
		app.AgentMounts[a.Name()] = mounted
		hubs.agents.add(a.Name(), a) // frontmatter agent: 的调用期解析
	}

	// 6. gateway 与 IM 通道(接线板)
	if ac.Serving.Addr != "" {
		agents := make([]serving.Runnable, 0, len(app.Agents))
		for _, a := range app.Agents {
			agents = append(agents, a)
		}
		app.Server = serving.New(ac.Serving.Addr, agents, logger)
		dispatcher := serving.NewDispatcher(logger)
		// app 层具名 stores 可被 suspend.store 以 cap://store/suspend/<name> 引用。
		// IM 通道与 HTTP /messages 共用同一挂起后端:飞书里挂起的审批
		// 从 HTTP 恢复(或反之)都成立,键只认会话。
		if kv, err := suspendKV(ac.Suspend, ac.Stores); err != nil {
			return nil, err
		} else if kv != nil {
			dispatcher.EnableSuspend(kv)
			app.Server.EnableSuspend(kv)
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
	} else if len(ac.Channels) > 0 {
		return nil, fmt.Errorf("channels configured but serving.addr is empty")
	}

	return app, nil
}

// buildAgentFromSpec 装配一个 agent:按关联顺序实例化 namespaces
// (跨 ns 引用在本 agent 的挂载集合内解析)→ 自动挂载全部导出 skill
// → 叠加全局兼容源的 include 选品 → 交给 buildAgent。
func buildAgentFromSpec(ctx context.Context, as *AgentSpec, app *AppConfig, global *source.Catalog,
	prompts *prompt.Resolver, defaultModel einomodel.ToolCallingChatModel,
	maxRisk capability.Risk, srcCache *sourceCache, hubs *skillHubs,
	opts BuildOptions) (*agent.Agent, *source.Catalog, error) {

	// 执行画像:agent 主循环 eff = app.merge(agent自己);也作为其
	// namespace/component 的 base(五级链的 app+agent 两级)。
	agentProfile := app.Profile.merge(as.Profile)
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 外部 skillpack 策略(app 级一份;namespace 的 use: 链接经 nsDeps 生效)
	packOpts, err := app.Skillpacks.options()
	if err != nil {
		return nil, nil, err
	}
	packRoot := app.Skillpacks.root(app.StateDir)

	// agent 的挂载目录:本 agent 关联的 namespaces 导出的 skill 落在
	// 这里,同时充当跨 ns cap://skill 引用的解析域(按关联顺序可见)。
	mounted := source.NewCatalog(maxRisk, logger)
	nsExports := newComponentExports() // 导出 component 注册表(本 agent 挂载序列共享)
	for _, mnt := range as.Mounts {
		nsCopy := mnt.NamespaceConfig // 按 agent 实例化,不共享装配产物
		err := buildNamespace(ctx, &nsCopy, nsDeps{
			exports: nsExports,
			global:  mounted, prompts: prompts, defaultModel: defaultModel,
			maxRisk: maxRisk, logger: logger,
			base:     agentProfile,      // app.merge(agent);ns 自己在 buildNamespace 内并入
			mount:    mnt.Override,      // per-mount 覆盖(最高优)
			appModel: app.Profile.Model, // 判断 component 是否需专属 model
			nsPath:   mnt.Path, srcCache: srcCache,
			packRoot: packRoot, packOpts: packOpts, execCfg: app.Exec, hubs: hubs,
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

	// 会话状态 / 治理边界:agent 未声明整块 → 回落 app 默认。
	inheritAppDefaults(app, &as.AgentConfig)

	m, err := agentModel(ctx, as.Profile.Model, defaultModel, agentProfile.retry())
	if err != nil {
		return nil, nil, err
	}
	a, err := buildAgent(ctx, &as.AgentConfig, agentProfile, caps, prompts, m, opts.Interactor, logger)
	if err != nil {
		return nil, nil, err
	}
	return a, mounted, nil
}
