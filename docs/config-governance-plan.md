# Config 治理:统一 schema + 分层降级

> **状态:已落地**(config 包)。核心结构 `config/profile.go` 的 `Profile`
> + 五级 `merge`;schema 各层内嵌 Profile;`config/{config,app,agent,namespace}.go`
> 按链解析;示例树(examples + smoke)与测试全部迁移,离线冒烟 e2e + `-race`
> 全绿。未纳入本次:digest.store 的**逐 component ctx 注入**(仍为主 loop
> 级 `SetResultBackend`,见下方步骤 6 备注)。

## 核心原则

一套配置 schema,声明在哪层就在哪层生效;缺失则沿
**app → agent → namespace → component** 逐级向上降级(**就近者胜**),
运行时解析。「在主 loop 执行 还是 在 component 执行」只是**在哪个解析点
读取画像**的区别,不是两套 schema。

现状的问题:agent 有自己一套(loop/session/digest/model…),component
的默认值却是另一套独立的 `defaults:` 口袋(max_steps/compaction/
tool_timeout/retry/digest_over…),两套字段名、两套语义,还没有 app 层
降级。本方案把它们合并成一条链。

## 降级链与解析点

```
app  ─┐
      ├─ agent ─┐
                ├─ namespace ─┐
                              ├─ component(inline,最近)
```

- **主 loop**(agent 及子 agent)读取:`app → agent自己` 的解析结果。
- **component / skill**(ns 内执行单元)读取:见下方五级优先。
- **编排步骤**(graph/workflow 的 step):`…→ component → step` 再加一层。

### 优先级:显式覆盖 > 具体声明 > 通用默认
agent 是集成方:挂载 namespace 时可**显式给该 namespace 指定配置**,压过
namespace/component 作者的选择(集成方最终话语权)。而 agent **自己的配置**
是主循环设置、兼作 component 的**通用默认**,排在具体声明之下。两个 agent
输入落在优先级两端:

**component 生效值(执行参数 loop/digest/reliability/steps),从高到低:**
```
1. agent 给该 namespace 的指定配置   (显式指令,最高)
2. component 自己的配置
3. namespace 的配置
4. agent 自己的配置                  (= 主循环配置,兼作 component 通用默认)
5. app 配置
                                      → 都没有则框架内置默认
```

**model 特例:能力不能自己指定 model(见 A 类说明),去掉 2、3 两层:**
```
1. agent 给该 namespace 的指定 model  →  2. agent 自己的 model  →  3. app 的 model
```
典型场景:agent 挂 catalog(便宜模型)+ research(强模型),靠 agent 给
各 namespace 指定 model 实现,component/namespace 自己不掺和。

### 降级粒度:by 配置项(per-field),不是 by 模块整体替换
逐**字段**沿链找最近声明——某层只声明块里的一个字段,不影响同块其它
字段继续向上降级。例:ns 只写 `loop.max_steps=3`、没写 `loop.compaction`,
component 执行时 max_steps=3(ns)、compaction=agent 的(继续往上找到)。

两级粒度:
- **容器块**(`loop / reliability / digest / steps`):字段各自独立降级。
- **策略叶子**(`model / compaction / retry`):整体 set-或-继承,不跨层拼
  半个策略(一份压缩/模型/重试策略是内聚的,混层会诡异)。

**实现前提**:所有可降级字段用指针/可判空(`*int` / `*CompactionConfig`
/ …),nil=继承、非 nil=本层生效——这样才能区分「没配」和「配成零值」。
这正是现有 `defaults:` 8 字段用指针的原因,推广到整个画像。

## 配置按「作用域」分四类

每个配置块声明它**降到哪一层为止**,这就是「config 治理」的规则表。

### A. 执行画像(execution profile)—— 全链降级 app→agent→ns→component
任何执行单元(主 loop 或 component)都要的执行参数,一套 schema、四层可声明:

| 块 | 字段 | 作用环节 | 可声明层 |
|---|---|---|---|
| `model` | provider/config | 执行单元的大脑 | **app / agent自己 / agent给ns指定**(namespace、component **不可**) |
| `loop` | max_steps, compaction | 循环迭代上限 + 上下文压缩 | 全部五级 |
| `reliability` | tool_timeout, retry | 工具/模型可靠性 | 全部五级 |
| `digest` | over, truncate, store | 大工具结果处理(消化+硬截断+暂存后端) | 全部五级 |
| `step_defaults` | timeout, retry | 编排步骤默认(有 steps 的单元) | 全部五级 |

> 落地注:步骤默认块用 **`step_defaults`** 而非 `steps`——`steps` 是编排族
> component 的**步骤列表**(结构声明),同名会与执行画像块在 YAML 里冲突;
> `step_defaults` 亦与 skill 层既有的 `step_defaults` 词汇一致。

**model 是特例:能力(component/skill)不能自己指定 model,namespace 也不
配 model**——model 是部署/成本决策,由集成方(agent/app)定,不由能力作者
钉死(能力作者硬编码 model 是部署耦合坏味道)。能力需要某种模型能力
(如视觉)是「requires」约束,另议,不是在这里 pin 具体模型。

**这一类取代现在的 `defaults:` 口袋**——`defaults:` 就是「执行画像在
agent/ns 层的声明」,并入统一 schema 后不再单列。
注:`compaction` 从 `session` 移到 `loop`——它压缩的是「执行单元循环的
工作上下文」,主 loop 和 component 都有,归 loop 才能全链降级。
`digest` 三项(over/truncate/store)全链降级到 component:每个执行单元从
自己解析的画像构建 ResultStore 经 ctx 注入,取代现在的进程级
`loop.SetResultBackend`(顺带解决多 agent 进程「后设者胜」)。
`stores`/`retrievers` 具名实例可在 **app 或 agent** 层声明(app = 全局
共享实例,如所有 agent 共用一个 redis session 实例)。

### B. 会话状态(conversation state)—— 主 loop 专属,app→agent 降级
只有主循环有「对话」;component 是无状态调用,没有:

| 块 | 字段 |
|---|---|
| `session` | store, window, record_tools, recall |
| `memory` | store, tools, scope, recall, seed |
| `todo` | store, enabled |

app 可设默认(如全局 session/todo 后端 redis),agent 覆盖;**不下沉到
component**。

### C. 治理边界(governance)—— agent 独占,app→agent 降级,不下沉
Ring 0 安全边界。它对 component 内部生效靠的是**运行时经 ctx 下沉**
(预算计入 skill 内部调用、审批对 skill 内部生效),不是配置下沉:

| 块 | 字段 |
|---|---|
| `approval` | mode, remember, rules |
| `budget` | max_model_calls, max_tokens |
| `structured_output` | … |

app 可设默认,agent 覆盖;namespace/component **不能放宽**(安全)。

### D. app 基础设施 —— app 专属,不降级
`secrets / catalog / serving / channels / suspend / observability / sources /
prompts`,以及 `agents`/`namespaces` 装配清单。identity/persona
(`name / description`、agent 的 `prompt: {system, loop}`)每层各有其名,
不降级。

## YAML 形态(同一 schema 出现在三层)

**app.yaml**——全局基线:
```yaml
# 执行画像默认(所有 agent/component 的基线)
model: {provider: minimax, config: {...}}   # = 原 default_model
loop: {max_steps: 30}
reliability: {tool_timeout: 60s, retry: {max_attempts: 3}}
digest: {over: 4000}
steps: {timeout: 30s, retry: 1}
# 会话/治理的全局默认(可选)
session: {store: cap://store/session/global}
budget: {max_tokens: 200000}
# app 基础设施
secrets: {...}
prompts: {sources: [...], default_label: prod}
catalog: {max_risk: mutating}
serving: {addr: ...}
agents: [agents/ops-manager.yaml, ...]
```

**agents/ops-manager.yaml**——覆盖 app + 主 loop 专属:
```yaml
description: ...
prompt: {system: {ref: ...}}         # persona(不降级)
model: {...}                          # 覆盖 app(省略=继承)
loop: {max_steps: 20}                 # 覆盖 app
# 会话状态(主 loop 专属)
session: {store: ..., window: 40, recall: {top_k: 3}, compaction: {...}}
memory:  {store: ..., tools: true}
todo:    {store: ...}
digest:  {over: 4000, truncate: 8000, store: ...}
# 治理(覆盖 app 默认,不被 ns 放宽)
approval: {mode: interactive, rules: [...]}
budget:   {max_model_calls: 60}
```

**namespaces/catalog.yaml**——该 ns 下 component 的画像默认:
```yaml
# 覆盖 agent、被 component 覆盖 —— 就是原 defaults:,并入统一 schema
model: {...}
loop: {max_steps: 8}
digest: {over: 3000}
steps: {retry: 1}
tools: [...]
components: [...]
skills: [...]
```

**component inline**——最近一层:
```yaml
components:
  - name: price_analyst
    engine: react
    model: {...}          # 覆盖 ns
    loop: {max_steps: 6}
```

## 这版替换掉什么

- **删掉独立的 `defaults:` 口袋**:它的 8 个字段(model/max_steps/
  compaction/tool_timeout/retry/step_timeout/step_retry/digest_over)并入
  统一执行画像;agent 层的 `loop/model/digest/reliability` 声明**一份两用**
  ——既配自己主循环,也作为其 component 的降级源。符合「无非是主 loop
  还是 component」。
- **app 首次获得完整执行画像默认层**(现在只有 `default_model` +
  半套 `reliability`)。`default_model` → `model`;`reliability` 保留但成为
  可降级画像的一部分。
- **compaction 从 session 移到 loop**(component parity);`reliability` 从
  app 专属变成全链可降级;`steps` 从 `defaults.step_*` 独立成块。

## agent 给 namespace 指定配置(per-mount override)
agent 挂载 namespace 时,mount 条目从「一个路径」升为「路径 + 覆盖画像」:
```yaml
# agents/x.yaml
namespaces:
  - path: ../namespaces/catalog.yaml
    model: {provider: minimax, config: {...}}   # 给这个 mount 显式指定
    loop:  {max_steps: 5}
  - path: ../namespaces/research.yaml
    model: {provider: openai, config: {...}}     # 另一个 ns 用强模型
```
这份覆盖画像是**最高优**(压过 component/namespace 自己的声明)。model 只能
在这里(或 agent 自己 / app)指定,namespace/component 内不可写 model。

**关键约束:per-mount override 只能指定 component 实际拥有的配置,即执行
画像 A 类(model/loop/reliability/digest/steps)。** component 根本没有的东西
——会话状态(session/memory/todo,主 loop 专属)、治理(approval/budget,
agent 独占)、component 的结构声明(engine/prompt/tools/params,是 component
自己的定义,不是执行参数)——**指定了对它无意义,故不在 override 里**。
所以 override 的形状 = 执行 `Profile`,不是完整 AgentConfig。这也让五级 merge
统一:component 生效值 = 五份**同形 Profile** 的就近合并。

## 实现要点(待确认后动手)

1. 定义统一 `Profile` 结构(model/loop/reliability/digest/steps),字段全指针
   /可判空,`merge(nearer)` 就近覆盖(推广现有 Defaults.merge)。
2. app/agent/namespace/component 各内嵌 `Profile`;agent 的 namespace mount 条目
   额外带一份**覆盖 Profile**(per-mount)。namespace/component 的 Profile 里
   剔除 model 字段(能力不可自指)。
3. component 生效值按五级 merge(高→低):
   `agent给该ns指定 → component → namespace → agent自己 → app`。
   主 loop 用 `merge(app, agent自己)`。model 走特例三级(去掉 ns/component)。
4. 会话状态(session/memory/todo)用 `merge(app, agent)`,不进 component。
5. 治理(approval/budget/structured_output)用 `merge(app, agent)`,ns/
   component 不参与。
6. **result 暂存后端改按执行单元注入**:每个单元从解析出的 `digest.store`
   构建 ResultStore、经 ctx 注入,删进程级 `loop.SetResultBackend`(digest.store
   得以降级到 component)。`stores`/`retrievers` 具名实例支持 app 层声明。
7. 删 `Defaults` 结构与 `defaults:` 键;namespace 文件去掉 `model:`;迁移示例
   + 测试;README config 段重写。

## 决策(已锁)

- **A**:`compaction` 从 `session` 移到 `loop`。坐实:主 loop 与 component 用
  的是**同一个** `loop.Compactor`(config.go:799 / skill.go:202),只是压各自
  循环的上下文;两个循环都有、只有主 loop 有 session,故归 loop 才能全链
  降级到 component。`loop` 最终 = `{max_steps, compaction}`。
- **B**:`session/memory/todo` 允许 app 层默认(app→agent,不下沉 component)。
  `digest` 本就是 A 类全链降级,app 默认天然覆盖 agent 与 component。
- **C**:`reliability` 并入统一画像,字段统一为 `{tool_timeout, retry}`
  (retry=RetryConfig;原 app 的 `model_retry` 改名 `retry`,与 component 侧的
  `defaults.retry` 合一)。

执行画像 A 类 = `model / loop / reliability / digest / steps`,全链降级。
