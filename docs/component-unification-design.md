# 组件统一设计方案

> 目标:所有"会喂给模型"的组件(循环族 skill·component / 编排族 steps /
> `use: model` 步骤)统一到同一套输入模型。三条正交通道 + 组件级输入隔离,
> 天然支持嵌套。现状见 [model-input-surface.md](model-input-surface.md);本文是
> 目标态设计。

---

## 0. 三句话总纲

1. **params(经 args 传入)→ prompt 占位符替换**,typed、按 `required` 校验。
   —— 指令的"参数化旋钮"。
2. **input → 用户消息**。—— 组件处理的自由数据。
3. **两个输入变量,层层隔离**:`{$input}` = 本组件的输入(每层独立);
   `{$user_input}` = loop 原始用户输入(内置,穿透所有嵌套恒定不变)。

三者正交:params 填指令、input 当数据、`{$user_input}` 兜底原始意图。

---

## 1. 统一的组件定义

```yaml
components:
  - name: analyze
    params:                               # ① 声明入参(typed,required 校验)
      company: {type: string, required: true}
      focus:   {type: string}             # 可选
    prompt: |                             # 指令 → 系统消息;params 占位符替换
      你是竞品分析助手,聚焦公司 {company}。{focus}
      背景(用户本轮原始问题):{$user_input}
    engine: react                         # 循环族;编排族则写 engine: graph + steps
    tools: [...]
```

- `prompt` 里:`{company}`/`{focus}` = 声明的 params(占位符);`{$input}` = 本
  组件输入;`{$user_input}` = loop 原始输入。
- 组件的 **input 不在定义里写死**——它在**使用点**传入,成为该组件的用户消息。

## 2. 统一的使用形态

任何调用点(编排 step、或组件被 model 当工具调),两条通道分明:

```yaml
- name: analyzeStep
  use: cap://component/common/analyze
  input: "{regionExtract}"          # ② → analyze 的用户消息 & analyze 内 {$input}
  args:                             # ① → analyze 的 params(占位符替换,required 校验)
    company: "{$user_input}"
    focus:   "关注增长引擎"
```

- `args`(映射)→ 填 analyze 声明的 params;缺 `required` 的 → **fail-fast**。
- `input`(模板)→ 渲染后成为 analyze 的用户消息;analyze 内以 `{$input}` 引用。
- 两个模板都在**调用方作用域**渲染(可用调用方的 `{$input}`/`{$user_input}`/
  `{步骤名}`/params)。

---

## 3. 变量命名空间(全局统一)

| 变量 | 含义 | 作用域 | 谁设置 |
|---|---|---|---|
| `{$input}` | **本组件的输入** | **每层独立**(进一层可被重设) | 调用点的 `input:`;未给则继承调用方 |
| `{$user_input}` | **loop 原始用户输入** | **全局恒定**(穿透所有嵌套) | agent.Run 一次性设定,不可变 |
| `{$user_id}` | 终端用户身份 | 全局 | runctx |
| `{参数名}` | 声明的 params | 本组件 | 调用点 `args` |
| `{步骤名}` | 上一步输出 | 本图内 | 编排引擎(仅 graph/workflow) |

**关键区分**(需求 3):`{$input}` 随嵌套变、`{$user_input}` 不变。顶层两者相等;
往下每传一次 `input:`,`{$input}` 切到新值,`{$user_input}` 始终是最初那句。

---

## 4. 运行时机制:输入怎么流、隔离怎么实现

### 4.1 两个 ctx 槽(runctx 扩展)

```go
// 一次性设定,immutable —— {$user_input}
runctx.WithLoopInput(ctx, s)   // 仅 agent.Run 顶层调用一次
runctx.LoopInput(ctx) string

// 作用域输入,可层层重设 —— {$input}
runctx.WithInput(ctx, s)       // 每个组件调用边界按 input: 重设
runctx.Input(ctx) string
```

顶层 `agent.Run`:两者都 = 用户原始输入。往下 `LoopInput` 永不再动;`Input` 在
每个组件调用边界按 `input:` 重设(未给 `input:` 则不动 = 继承调用方,向后兼容)。

### 4.2 两条通道各走各路

`capability.Invoke(ctx, cap, argsJSON)` 只有一个字符串通道,正好分工:

- **params** → `argsJSON`(JSON 对象,调用点 `args` 渲染而来);
- **input** → `ctx`(调用点 `input` 渲染后 `runctx.WithInput`)。

组件内部:`argsJSON` 解析出 params 填占位符;`runctx.Input(ctx)` 取本组件输入
(= `{$input}` + 用户消息);`runctx.LoopInput(ctx)` 取 `{$user_input}`。

### 4.3 调用边界统一动作(编排 step / 组件工具调 / 循环族 wrapper 都一样)

```
1. 在调用方作用域渲染 input:  → inputStr
2. 在调用方作用域渲染 args:   → argsJSON(params)
3. ctx = runctx.WithInput(ctx, inputStr)      // 重设 {$input};LoopInput 不动
4. capability.Invoke(ctx, cap, argsJSON)
   └─ 组件内:vars = params(argsJSON)
              vars["$input"]      = runctx.Input(ctx)      // 本组件输入
              vars["$user_input"] = runctx.LoopInput(ctx)  // 原始
              prompt.Render(vars) → 系统消息
              runctx.Input(ctx)   → 用户消息
```

隔离由 ctx 的作用域天然实现:`WithInput` 派生新 ctx,子组件看到新 `{$input}`,
返回后调用方的 ctx 不受影响;`LoopInput` 全程一个值。

---

## 5. 嵌套:逐层示例(核心)

场景:用户问 **「分析华东华南华北三个地区的音频销售」**。顶层 skill 扇出,每个
地区交给同一个 `regionReport` 组件处理。

```
agent.Run
  {$user_input} = "分析华东华南华北三个地区的音频销售"   ← 从此恒定
  {$input}      = 同上
   │
   └─ skill: multiRegion (engine: graph)
        step A: use: cap://component/.../regionReport
                input: "华东"                     ← 重设子组件 {$input}
                args:  {category: "音频"}
           ┌── regionReport 内部
           │     {$input}      = "华东"            ← 本组件输入(隔离)
           │     {$user_input} = "分析华东华南华北…" ← 原始,穿透
           │     {category}    = "音频"            ← param
           │     prompt(系统): "为 {$user_input} 这个整体任务,
           │                    专门分析 {$input} 地区的 {category} 品类"
           │     用户消息      = "华东"
           │     └─ 若 regionReport 再调子组件,同样机制再套一层
           └──
        step B: input: "华南"  → 另一份隔离的 {$input}="华南",{$user_input} 不变
        step C: input: "华北"  → 同上
```

要点:

- 三个 `regionReport` 各有隔离的 `{$input}`(华东/华南/华北),互不串扰;
- 三者的 `{$user_input}` **都是**最初那句,组件因此既能聚焦自己那份、又不丢
  全局意图;
- 再往下嵌套(regionReport 调子组件)时,`{$input}` 继续按 `input:` 重设、
  `{$user_input}` 继续穿透——**层数无关,规则不变**。

---

## 6. 各族如何映射到这套模型

| 构造 | prompt(系统) | input(用户消息) | params | `{$input}` / `{$user_input}` |
|---|---|---|---|---|
| 顶层 agent | L1+L2 persona+L3 | 用户输入 | — | 两者相等(顶层) |
| **循环族 component** | `prompt`(指令,params 占位符) | 使用点 `input:` | ✅ 占位符 | ✅ 均注入 |
| **编排族 step: `use: model`** | `prompt`(params 占位符) | 使用点 `input:` | ✅ 占位符 | ✅ 均注入 |
| 编排族 step: `use: tools/…` | — | — | args=工具入参 | 可在 args 引用 |
| 编排族 step: `use: component` | (转调下层组件) | `input:` 重设下层 | args=下层 params | 桥:调用方 → 下层 |

**统一效果**:循环族和 model 步骤都变成"prompt=系统指令(params 占位符)+
input=用户消息",`{$input}`/`{$user_input}` 处处可用。prompt 只需知道自己声明的
params 名(本就该知道),不需要知道 input 的内容——解耦。

---

## 7. 校验规则

1. **required params 未传 → fail-fast**(装配期能判则装配期,静态 step 尤其;
   动态 model 工具调用维持"回传消息让模型补"的自纠错类)。
2. **params 占位符**:声明的 param 应在 `prompt` 里有 `{param}` 占位符;是否强制
   "每个声明 param 必须有占位符 / 每个占位符必须有声明"—— 见开放问题。
3. `input:` 无类型、无占位符约束(它就是自由文本用户消息)。
4. `{$user_input}` immutable:任何 `WithLoopInput` 二次调用被忽略(防串改)。

---

## 8. 实现分期

| 阶段 | 内容 | 风险 |
|---|---|---|
| I | runctx 加 `LoopInput`(immutable)+ 保留 `Input` 作 scoped;agent.Run 顶层同时设两者 | 低 |
| II | `{$user_input}` 注入 graph vars + 循环族 brief;`{$input}` 循环族补齐(P1) | 低,向后兼容 |
| III | 组件调用边界接 `input:` 通道:step/组件调用渲染 input → `WithInput`;组件内 `Input`→用户消息 | 中 |
| IV | prompt→系统、input→用户 的角色切换铺到循环族 + model 步骤(硬切,新语法进错误消息) | 高,分期真机 |
| V | 校验:required fail-fast 统一;占位符规则定稿 | 中 |

建议 I→II 先做(小、安全,直接消除 `{$input}` 断层)。III→IV 是配置面硬切,
趁 1.0 前投,逐阶段真机验证。

---

## 9. 已定决策

1. **变量命名**:`{$input}` = 本组件输入(scoped);`{$user_input}` = loop 原始
   用户输入(恒定)。
2. **未给 `input:` 时**:继承调用方 `{$input}`(向后兼容)。
3. **params 占位符强制度**:**prompt 里每个 `{占位符}` 都必须是已声明的 param**
   (或内置 `$input`/`$user_input`/`$user_id`、或 graph 步骤名);未声明的裸占位符
   → 装配期报错。**反方向不强制**(声明了 param 但没用不报错)。外部 `cap://prompt`
   同样受此约束(D2:不放宽,带占位符的外部 prompt 需声明对应 param)。
4. **model 工具调用的 input**:默认**继承调用方 `{$input}`**(与 Claude Code
   typed-params 一致,不把 input 做进工具 schema)。

## 10. 多阶段引擎:输入怎么落 + 默认全透(D1 定论)

引擎的内部结构(阶段数、各阶段职责)**原样不动**。统一的只是"输入边界":
`args→占位符` / `input→用户消息(任务)` / `{$input}`·`{$user_input}` 两变量——
这套对**所有引擎**一致,与引擎有几个系统提示词无关。差别只是**提示词槽有几个**。

**默认全透**:`input`(任务)与 `params`(占位符)默认透到该引擎**每一个"会调
模型"的阶段**;工具执行子步(不调模型)不算阶段、不在范围。不做可关闭开关(YAGNI)。

| 引擎 | 会调模型的阶段(提示词槽) | input 透到 | params 可填 |
|---|---|---|---|
| react | 1:循环 | 该循环 | `prompt`(单) |
| direct | 1:单发 | 该单发 | `prompt`(单) |
| router | 1:路由决策 | 该决策 | `engine_config.route` |
| rewoo | 2:planner、solver(executor 跑工具不算) | 两段 | 两段 |
| reflection | 2:executor、reviewer | 两段 | 两段 |
| plan-execute | 3:planner、executor、replanner | 三段 | 三段 |

单阶段引擎(react/direct/router)"全透"退化为"就一处"。多阶段引擎(≥2 模型
阶段)才是这条决策的实际作用点。plan-execute 定稿示例:

```yaml
components:
  - name: researcher
    engine: plan-execute
    params: {topic: {type: string, required: true}}
    engine_config:                                       # 引擎结构不变,三段照旧
      planner:   "围绕 {topic} 把目标拆成最少可独立步骤。总目标:{$input}"
      executor:  "只完成给你这一步;总目标:{$input}(供 grounding)"
      replanner: "对照总目标 {$input} 判断:继续/完成/重规划"
    tools: [search, fetch]
# 使用
- use: cap://component/.../researcher
  input: "调研2024无线耳机降噪趋势"     # → goal,透到三个阶段
  args:  {topic: "降噪"}               # → {topic},填三段提示词
```

## 11. 诚实边界(D3/D4/D5,记录而非卖点)

- **D3**:`input:` 在 `use: component` 边界会**重设下层 `{$input}`**(re-scope),在
  `use: model` 步骤则**就是那条用户消息**(不 re-scope)。语义随 `use:` 目标略有
  差异。
- **D4**:params(占位符)与 input(用户消息)是**两个通道**,同一份数据放哪个是
  判断题——复杂度是"从耦合搬成二选一",非消除。规约:params=塑形指令的稳定旋钮,
  input=要处理的可变数据。
- **D5**:顶层 agent 结构更富(L1+L2 persona+L3+历史+Focus+记忆),**不是**与组件
  同一个 shape;"统一"指**组件之间统一 + 精神上向 agent 看齐**。

## 12. 实现分期与验证门

| 阶段 | 内容 | 硬切? | 验证门 |
|---|---|---|---|
| **P1(本批)** | runctx 加 `LoopInput`(set-once);`{$user_input}` 内置铺到 graph+循环族;`{$input}` 补进循环族任务书 | 否,纯新增向后兼容 | 全量测试 + 压测 + interactive |
| P2 | 组件调用边界接 `input:` 通道 + 组件级输入 re-scope(runctx.WithInput) | 中 | 同上 |
| P3 | `prompt`→系统、`input`→用户 角色切换(单系统引擎);多阶段全透 | 是,新语法进错误消息 | 同上 + example 迁移 |
| P4 | params 占位符必须声明的装配期校验;required fail-fast 统一 | 是 | 同上 |
| P5 | 文档/示例全量迁移 | 是 | 同上 |

**每阶段落地后跑:`go test ./...`(全量)+ 基准/压测 + interactive 真机**,绿了才进下一阶段。硬切阶段(P3+)额外迁移 examples 并把新语法嵌进 fail-fast 错误消息。
