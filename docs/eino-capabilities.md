# eino 能力索引(v0.9.12,基于官方中文文档全集通读)

> 用途:agent-kit 开发时的对照手册——eino 有什么、契约是什么、我们用没用、
> 自研件对应它哪个官方件。原文全集在本地 `~/Documents/Claude/Coding/eino-docs/`
> (cloudwego.github.io sparse clone,content/zh/docs/eino,102 篇)。
> 「对照」列:✅=已使用 ○=未用可用 ⚠=有坑见注 ✋=官方件与我们自研件同题。

## 1. 组件抽象(components/*)

| 能力 | 关键 API | 对照 |
|---|---|---|
| ChatModel | `BaseChatModel.Generate/Stream`;`ToolCallingChatModel.WithTools`(必须返回新实例,并发安全) | ✅ 五个 Ring 0 包装全实现;testmodel 返回自身(测试件豁免) |
| 模型公共 Option | `model.WithTemperature/MaxTokens/Model/TopP/Stop/Tools/ToolChoice`;实现方 `GetCommonOptions`;私有 option `WrapImplSpecificOptFn`+`GetImplSpecificOptions` | ○ 我们模型参数走构造配置,未暴露请求级 option |
| 自定义模型的 callbacks 义务 | 自行 `callbacks.OnStart/OnEnd/OnError/OnEndWithStreamOutput` + 实现 `Checker.IsCallbacksEnabled`;否则节点外自动包切面 | ⚠ 我们的包装不自报 → 节点级单 span,弹回的内层调用不可见;改进项:内层调用用 `ReuseHandlers` |
| Tool | `BaseTool.Info`(`ToolInfo{Name,Desc,ParamsOneOf}`);`InvokableTool.InvokableRun(ctx, argsJSON, opts)`;`StreamableTool`;v0.6+ `EnhancedInvokableTool`(多模态 `ToolResult`) | ✅ capability.AsTool;**无参工具 ParamsOneOf 必须为 nil,空 struct 部分模型 400**(FAQ) |
| 工具构造糖 | `utils.NewTool/InferTool`(struct tag 生成 schema)/`InferOptionableTool`/Enhanced 系列;`compose.GetToolCallID(ctx)` | ○ 我们手写 Meta;GetToolCallID 可用于工具内取调用 ID |
| ToolsNode | `ToolsNodeConfig{Tools, ToolAliases, UnknownToolsHandler, ToolArgumentsHandler, ExecuteSequentially(默认并行), ToolCallMiddlewares}` | ⚠ 防御四件套(名称幻觉/参数非法/别名/错误转结果)我们只配了 Tools;详见 §9 审计 |
| ChatTemplate | `prompt.FromMessages(FString/GoTemplate/Jinja2, ...)`、`MessagesPlaceholder` | ○ 我们自有 prompt.Value/Resolver |
| Embedding/Retriever/Indexer | `EmbedStrings` / `Retrieve(query, WithTopK/WithScoreThreshold/WithEmbedding)` / `Store(docs)` | ✅ retriever 用于召回;其余经 impl/source/vector |
| Document Loader/Parser/Transformer | `Load(Source)`;`parser.NewExtParser`;markdown/recursive/semantic splitter | ○ RAG 流水线可用 |
| Lambda | `InvokableLambda/StreamableLambda/CollectableLambda/TransformableLambda`、`AnyLambda`、`WithLambdaCallbackEnable`;内置 `ToList`、`MessageParser` | ✅ InvokableLambda 是能力→节点的桥 |
| **Agentic 变体(v0.9)** | `schema.AgenticMessage{ContentBlocks}`(reasoning/多模态/MCP/审批块;无 tool role);`AgenticModel=BaseModel[*AgenticMessage]`(**无 WithTools**,工具走请求级 option);`AgenticToolsNode`;ext: agenticopenai/ark/claude/gemini | ○ 官方建议:不需要原生 agentic 协议的存量应用留在 `*schema.Message` 路径——我们暂不迁 |

## 2. 编排(compose:Graph/Chain/Workflow)

| 能力 | 关键 API | 对照 |
|---|---|---|
| Graph | `NewGraph[I,O]`、`AddXXXNode/AddEdge/AddBranch`、`Compile→Runnable[I,O]`(Invoke/Stream/Collect/Transform 四态齐) | ✅ graph/workflow/rewoo 等引擎 |
| 类型对齐 | edge 上下游类型可赋值;`WithInputKey/WithOutputKey` 适配;扇入=各上游 Map 合并(可 `RegisterValuesMergeFunc`) | ✅ |
| 状态 | `WithGenLocalState[S]`、`WithStatePreHandler/PostHandler`(框架自动加锁)、`ProcessState` | ○ |
| 运行引擎 | `AnyPredecessor`=pregel(支持环)/`AllPredecessor`=dag;Chain 固定 pregel、Workflow 固定 dag;`WithEagerExecution` | ✅(默认) |
| Workflow | `NewWorkflow` + `AddInput(FieldMapping)` 字段级映射、控制/数据流分离(`WithNoDirectDependency`/`AddDependency`)、`SetStaticValue`;不支持环 | ○ 我们的 workflow 引擎是自研顺序钉死,可评估换 FieldMapping 版 |
| 嵌套 | `AddGraphNode`(同编译、callback 穿透)vs Lambda 封装 | ✅ |
| **外部变量只读原则** | Node/Branch/Handler 间是引用传递,禁止原地改(含 stream chunk) | ⚠ 审计通过:我们的 Modifier 只 append |

## 3. 流式(stream_programming_essentials)

- 四范式自动补全;**整体 Invoke → 内部全非流;整体 Stream → 内部全 Transform**,无局部智能路径。
- 自动转换仅两种:装箱(T→单帧流)与 Concat(需已注册 concat 函数,内置 Message/[]Message/string)。
- `StreamReader` 只能读一次、必须 Close;多消费方 `Copy(n)`,任一不 Close 阻塞资源释放;自产流 `schema.Pipe`,生产方 `defer sw.Close()` 且自 recover。
- 对照:✅ agent.Stream 的 Copy(2)+双侧 Close 合规。

## 4. Callbacks

- 五时机 Handler;`AppendGlobalHandlers`(**非并发安全,初始化一次**);运行时 `compose.WithCallbacks(h).DesignateNode/DesignateNodeWithPath`。
- `NewHandlerBuilder()`按时机;`utils/callbacks.NewHandlerHelper()`按组件类型(类型化 payload)。
- 嵌套调用换元数据:`callbacks.ReuseHandlers(ctx, newRunInfo)`;组件方兜底 `EnsureRunInfo`。
- Handler 间无顺序保证、payload 是共享引用禁改、一组触发完 RunInfo 清空。
- 对照:✅ observe.Progress 全局注入;○ 改进:digest/compactor/守卫内层模型调用加 ReuseHandlers 使其可观测。

## 5. CallOption

- 请求粒度配置直达节点:默认全局 → `WithChatModelOption/WithToolOption` 按类型 → `.DesignateNode`(仅顶层)/`.DesignateNodeWithPath`(跨嵌套)。
- 对照:○ 未用;若要"单次请求覆盖温度/工具",这是正路。

## 6. Checkpoint & Interrupt(compose)

- 静态断点 `WithInterruptBefore/AfterNodes`;`ExtractInterruptInfo(err)`。
- `CheckPointStore{Get/Set}`(KV!)+ `WithCheckPointID`;自定义类型 `schema.RegisterName`;`WithStateModifier` 恢复前改状态。
- v0.7+ 动态中断:`Interrupt/StatefulInterrupt/CompositeInterrupt`,节点内 `GetInterruptState[T]`/`GetResumeContext[T]`;定向恢复 `Resume/ResumeWithData/BatchResumeWithData`(interruptID 寻址)。
- 外部主动中断 `WithGraphInterrupt`(框架代存节点输入)。
- 对照:✋ **与我们 suspend(store.KV 快照)同题同构**——我们在 loop 层自研,eino 在 graph 层官方支持且有寻址/定向恢复。长期可评估:审批挂起改走 StatefulInterrupt + CheckPointStore(接口就是 KV,我们的 store.KV 直接适配)。

## 7. flow(compose 系 agent)

- **react.NewAgent**:`AgentConfig{ToolCallingModel, ToolsConfig, MessageModifier(每次调模型前,不持久), MessageRewriter(写回 state,先于 Modifier), MaxStep(默认12;一轮=2步), ToolReturnDirectly, StreamToolCallChecker(默认首非空包判定;先文本后工具的模型需自定义)}`;`WithMessageFuture()` 异步取中间消息;`BuildAgentCallback`。
- **host.NewMultiAgent**:Host 意图识别→Specialist 转发,多选并发 + Summarizer 汇总。
- 对照:✅ react 是我们主循环;⚠ MaxStep 语义(×2)与 StreamToolCallChecker 见 §9;○ WithMessageFuture 可替代部分进度观测;**官方立场(graph_or_agent):开放任务应使用 ADK,compose 模拟 ReAct 被类比"用 Word 写代码"——flow/react 仍维护但非主推,列为长期架构观察项**。

## 8. ADK(v0.5+,官方主推的 agent 运行时)

核心抽象:`TypedAgent[M]{Name/Description/Run→AsyncIterator[*AgentEvent]}`;`Runner`(checkpoint/callback/turnloop 只经 Runner 生效);`AgentEvent{Output, Action{Exit/Interrupted/BreakLoop/Transfer}}`;`TurnLoop`(push-based 多轮,抢占/优雅停止);`WithCancel`(安全点取消);`SessionValues`。

协作:**AgentAsTool(推荐)** `NewAgentTool`;Sequential/Parallel/Loop;Supervisor/Transfer **v0.9 已标不推荐**(上下文污染),主线 = ChatModelAgent + AgentTool / DeepAgent。

HITL:`Interrupt/StatefulInterrupt/CompositeInterrupt` + 地址寻址(`agent:A;node:g;tool:t`)+ `runner.Resume/ResumeWithParams` 定向恢复;跨进程恢复只需共享 Store。

**Middleware 全家桶(ChatModelAgentMiddleware,与我们自研件对照)**:

| eino middleware | 功能 | agent-kit 对应 |
|---|---|---|
| Summarization | 超阈值压缩历史;`Finalizer.PreserveSkills`(压缩后保留已加载技能) | ✋ loop.Compactor(我们有锚定/SafeCut;**PreserveSkills 值得借鉴**) |
| Skill | SKILL.md 渐进展示 + inline/fork/fork_with_context | ✋ skillpack(方案见 docs/eino-skill-integration.md) |
| PlanTask | TaskCreate/Get/Update/List 四工具,文件后端,依赖图+环检测 | ✋ todo(我们有校验/收口/PlanSection;它有任务间依赖) |
| ToolSearch | 大工具库动态选择(元工具/模型原生 deferred) | ✋ catalog include 选品(静态);动态选品可借鉴 |
| ToolReduction | 大结果截断 offload 文件 + 旧轮工具消息清理 | ✋ digest(over/truncate/read_result) |
| PatchToolCalls | 修复悬空 tool_call(插占位 tool 消息) | ✋ 我们 RepeatBreak 弹回手工回填 tool 消息同技;可借其"放链最前"的兜底思路 |
| AgentsMD | CLAUDE.md 式规约注入(@import 递归) | ✋ PromptLayers L2/记忆注入 |
| Filesystem | ls/read/write/edit/glob/grep + execute;backend: InMemory/Local/**火山 Agentkit 云沙箱** | ✋ pack_read 囚笼 + exec.Sandbox(我们沙箱可强制,它取决 backend) |

预制:**DeepAgents**(`deep.New`:规划+文件系统+子 agent 委派,ResumableAgent);Plan-Execute;ModelRetry/Failover(`ShouldRetry` 可拒绝不合格输出并改写下次输入——比我们 RetryModel 强,值得借鉴)。

## 9. 对 agent-kit 的审计结论(回文档详细检查)

1. **✅ 合规**:react Modifier/Rewriter 分工与顺序、WithTools 不可变性、工具错误
   作结果回传(避免 ToolsNode 错误炸轮次)、stream Copy/Close、全局 callbacks
   一次性注入、外部变量只读。
2. **✅ 已修 MaxStep 语义**:max_steps 对外语义改为"工具调用轮数",react
   装配换算 2N+1(BuildReAct);配置 N = N 轮工具调用 + 一次收尾作答。
3. **✅ 已修 无参工具 ParamsOneOf**:capability.NoParams 标记 → ToolInfo
   置 nil;todo_read 采用。
4. **✅ 已修(部分)ToolsNode 防御**:UnknownToolsHandler 已配(工具名幻觉
   转结果自纠);ToolArgumentsHandler 未配——各工具自行解析参数并以友好
   错误回传,暂无需求。
5. **✅ 已修 StreamToolCallChecker**:自定义 checker 读到工具调用或 EOF 才
   判定(兼容先文本后工具调用的模型);代价是纯文本终答的分支判定等到
   流收尾。
6. **✅ 已修 观测改进**:loop.observedGenerate 给内层模型调用挂
   ReuseHandlers + OnStart/OnEnd 切面(digest 摘要、压缩摘要、三守卫弹回),
   进度/tracing 可见独立子 span;testmodel.WithTools 也已按契约返回共享
   内核的新实例。
7. **✋ 长期**:suspend↔compose checkpoint(KV 同构)、Compactor↔Summarization
   (PreserveSkills)、RetryModel↔ModelRetryConfig(ShouldRetry 语义)、
   动态工具选品↔ToolSearch。升级 eino 版本时逐项对照。

## 10. 版本演进速查

v0.1 首发(组件+编排+流+callback)→ v0.2 Host 多 agent → v0.3 开源/原生 State
→ v0.4 eager dag/JSONSchema(**移除 GetState**)→ v0.5 **ADK 首发** →
v0.6 移除 OpenAPI3(**ParamsOneOf 改 JSONSchema**)→ v0.7 **中断恢复重构**
(寻址/定向恢复)→ v0.8 **middleware 体系**(含 Skill;filesystem 多处
breaking)→ v0.9 **agentic runtime**(AgenticMessage/Cancel/TurnLoop/
Failover;Supervisor/Transfer/Workflow-agents 降级不推荐)。
