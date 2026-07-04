# Config 治理:统一 schema + 分层降级

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

- **主 loop**(agent 及子 agent)读取:`app → agent` 的解析结果。
- **component / skill**(ns 内执行单元)读取:`app → agent → namespace →
  component` 的解析结果。
- **编排步骤**(graph/workflow 的 step):`…→ component → step` 再加一层。

解析伪码(现有 `Defaults.merge` 的推广,贯穿四层):
```
resolve(field, unit):
  沿 [component, namespace, agent, app] 从近到远,取第一个显式声明的;
  都没有 → 框架内置默认。
```

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

| 块 | 字段 | 作用环节 |
|---|---|---|
| `model` | provider/config | 执行单元的大脑 |
| `loop` | max_steps, compaction | 循环迭代上限 + 上下文压缩 |
| `reliability` | tool_timeout, retry | 工具/模型可靠性 |
| `digest` | over, truncate, store | 大工具结果处理(消化+硬截断+暂存后端) |
| `steps` | timeout, retry | 编排步骤默认(有 steps 的单元) |

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

## 实现要点(待确认后动手)

1. 定义统一 `Profile` 结构(model/loop/reliability/digest/steps),字段全指针
   /可判空,`merge(nearer)` 就近覆盖(推广现有 Defaults.merge)。
2. app/agent/namespace/component 各内嵌 `Profile`(inline);解析时按
   `[component, ns, agent, app]` 链 merge。
3. 主 loop 用 `merge(app, agent)`;component 用
   `merge(app, agent, ns, component)`。
4. 会话状态(session/memory/todo)用 `merge(app, agent)`,不进 component。
5. 治理(approval/budget/structured_output)用 `merge(app, agent)`,ns/
   component 不参与。
6. **result 暂存后端改按执行单元注入**:每个单元从解析出的 `digest.store`
   构建 ResultStore、经 ctx 注入,删进程级 `loop.SetResultBackend`(digest.store
   得以降级到 component)。`stores`/`retrievers` 具名实例支持 app 层声明。
7. 删 `Defaults` 结构与 `defaults:` 键;迁移示例 + 测试;README config 段重写。

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
