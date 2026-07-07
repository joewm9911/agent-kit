// agent.go:AgentConfig → agent.Agent 的装配(单文件 Build 与多文件 BuildApp
// 共用),含模型/存储/召回的解析辅助。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/askuser"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/memory"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/runtime/suspend"
	"github.com/joewm9911/agent-kit/todo"
)

// agentModel 解析 agent 主循环的模型:own(agent 自己 model:,可为 nil)
// → fallback(app 层 model,已包装)。agent 显式声明时在此套 Ring 0 中间件
// (retry 用其解析出的 reliability.retry + 预算);未声明则复用共享的 app
// 默认(已包装,零重建)。namespace/component 不参与——它们不可自指 model。
func agentModel(ctx context.Context, own *ModelConfig,
	fallback einomodel.ToolCallingChatModel, retry loop.RetryConfig) (einomodel.ToolCallingChatModel, error) {

	if own == nil {
		if fallback == nil {
			return nil, fmt.Errorf("no model (declare agent model or app-level model)")
		}
		return fallback, nil
	}
	m, err := model.Build(ctx, own.Provider, own.Config)
	if err != nil {
		return nil, err
	}
	// 质量守卫已上移至循环装配(ReviewModel,见 buildAgent/skill.Build);
	// 模型链只负责预算记账与瞬时错误重试。
	return loop.BudgetModel(loop.RetryModel(m, retry)), nil
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

// resolveKV 解析 store 引用并构建一个 KV 后端(ref 为空 → inmemory 默认;
// 裸 type 时沿用就地 bareConf)。由装配层调用,后端注入进各自的消费方
// (todo 对象 / agent 结果暂存),不再推进任何进程级全局。
func resolveKV(ref string, bareConf map[string]any, stores []StoreInstance, wantKind string) (store.KV, time.Duration, error) {
	typ, conf, ttl, err := resolveStoreRef(ref, stores, wantKind)
	if err != nil {
		return nil, 0, err
	}
	if conf == nil {
		conf = bareConf
	}
	kv, err := store.NewBackend(typ, conf) // typ 空 → inmemory
	if err != nil {
		return nil, 0, fmt.Errorf("%s store backend: %w", wantKind, err)
	}
	return kv, ttl, nil
}

// suspendKV 解析挂起持久化后端:store 槽(裸 type 或 cap://store/suspend/
// <name>)优先,dir 是 file 后端简写;两者都空 → 不启用(返回 nil)。
func suspendKV(sc SuspendConfig, stores []StoreInstance) (store.KV, error) {
	ref, bare := sc.Store, sc.StoreConfig
	if ref == "" {
		if sc.Dir == "" {
			return nil, nil
		}
		ref, bare = "file", map[string]any{"dir": sc.Dir}
	}
	kv, _, err := resolveKV(ref, bare, stores, "suspend")
	if err != nil {
		return nil, fmt.Errorf("suspend: %w", err)
	}
	return kv, nil
}

// componentTodo 为组件级调用清单构造一个进程内后端并包成 Todo:组件清单
// 是调用级临时草稿(结束即弃),不需外置/分布式,inmemory 即可。装配层
// 构造并注入,组件本身不感知后端。
func componentTodo() *todo.Todo {
	kv, _ := store.NewBackend("inmemory", nil) // inmemory 恒可用(store 包 init 常驻)
	return todo.New(kv, 0)
}

// buildAgent 用已选品的能力面、已解析的模型与已解析的执行画像 eff
// (= app.merge(agent自己))装配 agent。eff 提供全部执行画像 A 类参数
// (max_steps/compaction/tool_timeout/retry/digest);会话状态与治理边界
// 直接读 ac(调用方已把 app 层默认并入)。
func buildAgent(ctx context.Context, ac *AgentConfig, eff Profile, caps []capability.Capability,
	prompts *prompt.Resolver, m einomodel.ToolCallingChatModel,
	interactor runctx.Interactor, logger *slog.Logger) (*agent.Agent, error) {

	if logger == nil {
		logger = slog.Default()
	}

	if m == nil {
		return nil, fmt.Errorf("no model (declare agent model or app-level model)")
	}

	// 存储后端:各自解析后注入消费方(不再有进程级全局),同进程多 agent
	// 各持各的后端,互不覆盖。todo 仅启用时构造;result 仅消化启用时构造。
	todoOn := ac.Todo.Enabled == nil || *ac.Todo.Enabled
	var td *todo.Todo
	// 收口检查(Ring 0,只包主循环模型——digest/压缩的摘要 Generate 也是
	// 纯文本收尾,包上会被误弹回;skill 子循环的临时清单随调用即弃):
	// - DeniedCallsCheck:本轮有被用户拒绝的调用 → 终答必须如实区分,
	//   不得声称全部完成(实测模型会把被拒调用标成已完成);
	// - todo.FinishCheck:计划未收口 → 弹回补交(仅 todo 启用时)。
	finishChecks := []func(context.Context) string{loop.DeniedCallsCheck}
	if todoOn {
		kv, ttl, err := resolveKV(ac.Todo.Store, ac.Todo.StoreConfig, ac.Stores, "todo")
		if err != nil {
			return nil, err
		}
		td = todo.New(kv, ttl)
		caps = append(caps, td.Capabilities()...)
		finishChecks = append(finishChecks, td.FinishCheck)
	}
	// 统一评审循环(ReviewModel):重复终止 → 收口守卫 → 业务收口检查,
	// 顺序显式、全局重试预算,取代旧的三层包装嵌套(乘法放大无全局闸)。
	loopModel := loop.ReviewModel(m,
		loop.RepeatBreakReviewer(), loop.FinishReviewer(), loop.CheckedReviewer(finishChecks...))
	var resultKV store.KV
	var resultTTL time.Duration
	if eff.digestOver() > 0 {
		kv, ttl, err := resolveKV(eff.Digest.Store, eff.Digest.StoreConfig, ac.Stores, "result")
		if err != nil {
			return nil, err
		}
		resultKV, resultTTL = kv, ttl
	}

	if ac.Capabilities.AskUser == nil || *ac.Capabilities.AskUser {
		caps = append(caps, askuser.New())
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
	var kv memory.Store
	if ac.Memory.ToolsLegacy != nil {
		return nil, fmt.Errorf("agent %s: memory.tools 已改名 expose_tools(避免与工具列表语义的 tools 冲突)", ac.Name)
	}
	if ac.Memory.ExposeTools || kvK > 0 {
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
		if ac.Memory.ExposeTools {
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
	// budget/approval 运行态后端:不配 store 留进程内(单副本默认),
	// 配了(如 redis)则账目/决策记忆跨副本一致。
	var approvalKV store.KV
	var approvalTTL time.Duration
	if ac.Approval.Store != "" {
		var err error
		if approvalKV, approvalTTL, err = resolveKV(ac.Approval.Store, ac.Approval.StoreConfig, ac.Stores, "approval"); err != nil {
			return nil, err
		}
	}
	approval, err := loop.NewApprovalState(mode, ac.Approval.ApprovalPolicy, approvalKV, approvalTTL)
	if err != nil {
		return nil, err
	}
	var budgetKV store.KV
	var budgetTTL time.Duration
	if ac.Budget.Store != "" {
		var err error
		if budgetKV, budgetTTL, err = resolveKV(ac.Budget.Store, ac.Budget.StoreConfig, ac.Stores, "budget"); err != nil {
			return nil, err
		}
	}
	if eff.digestOver() > 0 {
		caps = append(caps, loop.ReadResult()) // 消化结果的原文取回
	}
	caps = loop.TimeoutTools(caps, eff.toolTimeout().Std())
	caps = loop.DedupCalls(caps)                            // 重复调用断路器(同轮同参打转拦截)
	caps = loop.DigestResults(caps, m, eff.digestOver())    // 大结果消化
	caps = loop.TruncateResults(caps, eff.digestTruncate()) // 工具结果硬截断(Ring 0)
	caps = suspend.DurableEffects(caps)                     // 效果日志(挂起恢复的重放不二次执行)
	caps = loop.GateApprovalCtx(caps)
	caps = loop.ControlTools(caps) // 中断/插话检查点(审批之外:中断时不再询问)
	if todoOn {
		caps = td.Nudge(caps) // 计划卡住提醒(harness 强制纪律)
	}
	caps = loop.RecordTools(caps)   // 轨迹记录(最外层:记模型实际看到的)
	caps = loop.ProgressTools(caps) // 进度事件发射(体感时长:含审批等待)

	// 上下文压缩归执行画像(loop.compaction),主 loop 从解析出的 eff 取;
	// comp 是本地副本,ResolvePrompt 在其上锁版本,供 Compactor 与 agent 复用。
	comp := eff.compaction()

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
			if comp.Enabled() || sessK > 0 {
				logger.Warn("session store lacks FullLoader: rolling summary and beyond-window recall are DISABLED",
					slog.String("agent", ac.Name), slog.String("store", ac.Session.Store))
			}
		}
		// 窗口必须容得下摘要视图:否则滚动摘要(+锚定)会被窗口裁剪
		// 静默切掉,跨轮记忆凭空消失。+2 = 摘要 + 锚定两条合成消息。
		if comp.Enabled() && ac.Session.Window < comp.Keep()+2 {
			return nil, fmt.Errorf("session.window (%d) must be >= loop.compaction keep_recent+2 (%d), or the rolling summary gets trimmed away",
				ac.Session.Window, comp.Keep()+2)
		}
	}

	// 摘要提示词(内容策略可配置,归并指令框架追加):装配期解析锁版本
	if err := comp.ResolvePrompt(ctx, prompts); err != nil {
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
	// Focus 只在主循环开:skill/component 子循环的目标是 args,不是外层用户原话。
	layers := loop.PromptLayers{Loop: loopTpl.Text, Persona: personaTpl.Text, Focus: true}
	if todoOn {
		layers.Plan = td.PlanSection // 计划每轮注入消息尾部(harness 强制可见)
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
		Model:        loopModel, // 主循环模型(todo 启用时带计划收口守卫)
		Capabilities: caps,
		MaxSteps:     eff.maxSteps(),
		Modifier:     layers.Modifier(),
		Rewriter:     loop.Compactor(m, comp), // 压缩摘要用裸 m,不受收口守卫弹回
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
		Store: sessStore, Window: ac.Session.Window, Compaction: comp,
		Structured: enforcer, Interactor: interactor,
		Approval: approval, Budget: loop.NewBudgetGate(ac.Budget.BudgetConfig, budgetKV, budgetTTL),
		RecordTools: record,
		ResultKV:    resultKV, ResultTTL: resultTTL,
	}), nil
}

// autoRecall 是 L4 自动召回钩子:每次模型调用前,用本轮用户输入
// 检索长期记忆(KV,策略在后端)与窗口外的会话历史(策略在注册的
// Retriever),两路独立配置,命中片段注入消息尾部第四层。
// 会话历史来自 agent 本轮已加载的全量记录(ctx 共享,一轮只读一次
// store);同轮多次模型调用的查询不变,结果按轮 memo,不重复检索。
func autoRecall(kv memory.Store, scope memory.ScopeConfig, retr session.Retriever, window, sessK, kvK int) func(ctx context.Context) []string {
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
