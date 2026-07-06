# eino 对照体检:agent-kit 的误用与加强清单

> 基于 eino v0.9.12 官方文档全集(本地 `~/Documents/Claude/Coding/eino-docs/`)
> 与 agent-kit 当前代码的逐项对照。姊妹篇:`eino-capabilities.md`(能力索引,
> 查"eino 有什么");本文回答"我们哪里用错了、哪里该加强"。
> 状态标记:🔴 误用待修 🟡 建议加强 🟢 已修正(留档防回退) ⚪ 观察项。

## 1. 误用(与 eino 契约相悖)

### 🔴 M1 工具参数解析失败以 error 返回 → 整轮死亡

eino 契约:ToolsNode 对工具返回的 error **透明传播**,react 循环随之整轮
失败。我们的哲学是"错误作结果回传,让模型自纠"——但 exectool、httptool、
todo_write 等在 `json.Unmarshal(argsJSON)` 失败时 `return "", err`
(exectool.go:217 / httptool.go:128 / todo.go:175)。模型幻觉出坏 JSON 的
那一刻,不是自纠而是整轮报错。

**修法(官方 FAQ 的防御四件套之一)**:BuildReAct 的 ToolsNodeConfig 加
`ToolCallMiddlewares`,统一把工具执行 error 转成结果字符串;**必须先放行
`compose.IsInterruptRerunError`**(否则 HITL 中断会被吞)。一处接线,全部
工具对齐哲学,且未来新工具无需各自记住这条纪律。

### 🟢 M2 Stream 返回流的 Close 契约(核对通过)

`StreamReader` 只能读一次、所有消费方必须 Close。agent.Stream 内部
Copy(2) 的第二份已 `defer Close`;调用端核对:serving.go:156 与
dispatcher.go:235 均 `defer sr.Close()` ✅。留一条纪律:**新增任何流
消费方(新通道/新 handler)必须 defer Close**,漏关会阻塞源流释放。

### 🟢 已修正(留档防回退,详见 eino-capabilities.md §9)

| 项 | 曾经的误用 | 修正 |
|---|---|---|
| MaxStep 语义 | max_steps 直传 eino(一轮=2步),配 20 只有 10 轮 | 换算 2N+1,对外=轮数(react.go) |
| 无参工具 | ParamsOneOf 传空 map,部分厂商 400 | capability.NoParams → nil |
| 工具名幻觉 | ToolsNode 默认报错炸轮 | UnknownToolsHandler 转结果自纠 |
| 流式工具判定 | 默认首包判定,先文本后调用的模型误判终答 | 自定义 StreamToolCallChecker 读到调用或 EOF |
| 嵌套模型调用 | digest/压缩/守卫弹回对 callbacks 不可见 | observedGenerate:ReuseHandlers + 自报 OnStart/OnEnd |
| WithTools 可变性 | testmodel 返回自身 | 共享内核新实例 |
| 全局 handler | ——(一直合规:初始化一次) | —— |

## 2. 需要加强——compose 系可直接接的官方能力

### 🟡 S1 Checkpoint & Interrupt(与 suspend/approval 同题,官方件更完整)

compose 的 `CheckPointStore{Get/Set}` 接口与我们的 store.KV **同构**,
v0.7+ 的动态中断有我们没有的能力:中断点寻址(`InterruptCtx.ID/Address`)、
**定向恢复**(`ResumeWithData` 按中断点投递数据)、`StatefulInterrupt`
(中断携带内部状态)、外部主动中断(框架代存节点输入)。我们的
suspend(KV 快照)+ 审批挂起是自研等价物,但没有寻址与定向恢复——
多个审批点同时挂起时只能整体恢复。
**建议**:审批挂起类 HITL 评估迁移到 `StatefulInterrupt` + `WithCheckPointStore`
(store.KV 直接适配);suspend 的效果日志(DurableEffects)保留——官方
checkpoint 不覆盖"重放不二次执行"。中期项,动之前先写迁移设计。

### 🟡 S2 ToolsNode 防御四件套的剩余两件

已接 UnknownToolsHandler;未接:`ToolArgumentsHandler`(参数预处理/修复,
配合 M1 的 middleware 可做坏 JSON 的抢救性修复)与 `ToolAliases`
(工具改名后的兼容映射——技能重命名时对旧会话历史里的调用名兼容)。
小改动,随 M1 一起做。

### 🟡 S3 CallOption 请求级配置未暴露

eino 的三级分发(全局 → 按组件类型 → DesignateNode/Path)我们完全没用:
模型参数只有构造期配置,无法"这一次调用降温度/换工具集"。serving 场景
(每请求覆盖)会需要。**建议**:agent.Run 加 opts 透传
`compose.WithCallbacks/WithChatModelOption` 一类;不急,接口预留即可。

### ⚪ S4 Workflow 字段级映射(评估后不做)

官方 `NewWorkflow` 的 FieldMapping 是**结构体字段级**映射(反射,要求
导出字段)。评估结论:我们 skill steps 的数据面是 JSON 字符串整值传递
+ 模板替换,没有结构化字段可映射——FieldMapping 没有受体,换底收益≈0,
纯迁移成本。登记触发条件:component 间开始传结构化对象(而非 JSON
字符串)时再评估。

### 🟡 S5 观测生态直连

eino-ext 有现成 callbacks 实现(Langfuse / CozeLoop / APMPlus),与
observe.Progress 并挂即得生产级 tracing(嵌套 span 我们已经自报,接上
就能看到守卫弹回/digest 的完整树)。`GraphCompileCallback` 可导出拓扑
生成 mermaid(devops 可视化)。低成本高收益,建议 examples 里做一个
Langfuse 接线示例。

### ⚪ S6 其他登记

- `WithMessageFuture`:异步取 react 中间消息,可作全局 callbacks 之外的
  第二条进度通道(serving SSE 场景更顺)。
- `NewStreamGraphBranch`:流式分支抢首包决策,自研 graph 引擎如需流式
  branch 用它。
- 无内置批处理节点:官方建议动态建图或自定义 BatchNode(compose/batch
  示例);我们 bulk 类场景目前靠 rewoo,够用。

## 3. 需要加强——ADK 侧只能"借设计"的(我们是 compose 系,middleware 接不进)

| eino ADK 件 | 值得借的设计 | 对应自研件 | 优先级 |
|---|---|---|---|
| Summarization | **Finalizer.PreserveSkills**:压缩后保留已加载技能内容。复核后**当前架构不适用**:我们的技能正文进子循环 persona,主循环压缩不可及——这是 inline 模式(技能内容作为工具结果进主上下文)才有的病,待 P2 inline 落地时一并做 | loop.Compactor | 随 inline |
| ModelRetryConfig | **ShouldRetry 语义否决**:不只重试网络错,还能"拒绝不合格输出并改写下次输入、覆盖退避"——比我们 Transient 启发式强一档;我们的守卫弹回(FinishGuard 系)其实是它的特化 | RetryModel + 守卫 | 中 |
| ToolReduction | **已借最小版**:Clear 阶段落地为 compaction 的 tool_clear(保护窗外超长 tool 消息替换占位,零模型调用,先清后摘;digest 指针跳过)。offload 成文件的形态我们用 digest+read_result 等价 | digest + compaction.tool_clear | ✅ 最小版已落地 |
| PlanTask | 任务**依赖图**(blocks/blockedBy + DFS 环检测) | todo | 低(交互场景暂不需要) |
| ToolSearch | 大工具库动态选品(元工具检索 / 模型原生 deferred tools) | catalog include(静态) | 工具面 >40 时再做 |
| PatchToolCalls | 悬空 tool_call 的**开链兜底**(修复历史里 assistant 有调用无结果的消息,HITL 取消/恢复丢失场景) | RepeatBreak 的 tool 消息回填是同技的局部应用 | 中(做挂起恢复时一起) |
| AgentsMD | `@路径` 递归 import(深度 5/环检测/去重) | prompts/PromptLayers | 低 |
| DeepAgents | task 工具的**上下文隔离委派**(子 agent 不见历史,只回结果)——我们 skillpack fork 语义已对齐,登记即可 | skill fork | —— |

## 4. 架构级观察项(不动手,记录触发条件)

- **graph_or_agent 官方立场**:开放型 agent 主推 ADK,compose 模拟 ReAct
  被类比"用 Word 写代码";flow/react 在 v0.9 仍维护但非演进主线。
  **触发重估的信号**:flow/react 停止跟进新能力;或我们需要 TurnLoop 级
  抢占(push-based 多轮 + 安全点取消)、寻址式 HITL 时。skill 资产已
  双向兼容(einoskill 适配器),届时迁移不被技能层拖住。
- **AgenticMessage 路径**:官方建议存量应用留在 `*schema.Message`;
  当需要原生 reasoning 块/多模态工具结果/MCP 审批块时再评估。
- **升级纪律**:每次升 eino 版本,对照 release_notes_and_migration/
  (v0.6 移除 OpenAPI3、v0.8 filesystem 多处 breaking、v0.9 middleware
  接口加方法都咬过人);本文与能力索引随升级重审。

## 5. 行动清单

| # | 项 | 规模 | 建议 |
|---|---|---|---|
| ✅ | M1 ToolCallMiddlewares 错误转结果(放行 InterruptRerun);S2 的 ToolArgumentsHandler/ToolAliases 暂无场景,登记不接 | 小 | 已完成 |
| ✅ | M2 serving/通道侧流 Close 核对 | 审查 | 已通过(serving/dispatcher 均 defer Close) |
| ✅ | M1 已落地:react 装配统一挂 ToolCallMiddlewares(错误转结果,放行 InterruptRerun),覆盖主循环与全部技能/组件子循环;行为测试钉住"坏 JSON 不炸轮" | 小 | 已完成 |
| ✅ | S5 已落地:interactive 可选接入 Langfuse(LANGFUSE_PUBLIC_KEY/SECRET_KEY 即启用,与进度切面并挂) | 小 | 已完成 |
| — | PreserveSkills:复核不适用(见上表),随 inline 模式再做 | — | 搁置 |
| P1 | RetryModel 引入 ShouldRetry 语义否决(与守卫弹回统一) | 中 | 设计后做 |
| P2 | S1 审批挂起迁 StatefulInterrupt + CheckPointStore(迁移设计先行) | 大 | 评审 |
| P2 | S3 CallOption 透传、S4 FieldMapping、ToolSearch、PatchToolCalls | 各小-中 | 按需 |
