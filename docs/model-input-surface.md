# 模型输入配置面:现状与统一方案

> 目的:把"配置如何把输入喂给模型"这件事,在所有构造(顶层 agent / 循环族
> skill·component / 编排族 steps)上摊平,定位偶然复杂度,给出统一目标态。
> 现状部分均对照代码(file:line),目标态部分是**提案**,待评审。

---

## 1. 现状:四类输入机制

引擎有 8 种(react/direct/rewoo/router/plan-execute/reflection + graph/workflow),
但**引擎数量不是配置复杂度来源**——6 个循环引擎对外的输入接口完全一样(都在
`skill.go` 的 `brief.Render` 层),引擎只决定内部怎么处理任务。配置面真正的分裂
是下面四类:

图例:`[SYS]` = 进系统消息,`[USR]` = 进用户消息。

### A. 顶层 Agent(engine: react,唯一)

```yaml
agents:
  - name: assistant
    prompt:
      loop:   "..."          # L1 框架规约
      system: "你是销售助手"  # L2 persona
    tools: [get_sales, ...]
```

| 项 | 传递路径 | 代码 |
|---|---|---|
| `prompt.system`(persona) | → **[SYS] 头部 L2** | agent.go:337 |
| `prompt.loop` + 环境 | → **[SYS] 头部 L1 + L3** | prompt.go:106-147 |
| 用户输入 | → 会话历史 user + 尾部 Focus 重述 | prompt.go:172-183 |
| **args** | 无 | — |
| **`{$input}`/`{$user_id}`** | ✅ 有(runctx → L3 + Focus) | prompt.go:173 |
| 消息结构 | `[SYS(L1+L2+L3), …历史, 记忆/计划/Focus]` | prompt.go:148 |

### B. 循环族 skill/component(engine: react|direct|rewoo|router|plan-execute|reflection)

```yaml
components:
  - name: analyze
    engine: react            # 换 rewoo/router/... 输入接口不变
    params:
      company: {type: string, required: true}
    prompt: |                # 任务书(brief)
      分析公司 {company} 的情况。原始问题:{input}
    tools: [...]
```

| 项 | 传递路径 | 代码 |
|---|---|---|
| `prompt`(任务书) | → **[USR] 用户消息**(⚠️ 不是 system;system 只有 L1,无 persona) | skill.go:244-248 |
| **args = JSON 对象** `{"company":"X"}` | 每键 → `{param}` | skill.go:237-240 |
| **args = 裸字符串** | 整串 → `{input}` | skill.go:241-242 |
| **`{$input}`/`{$user_id}`** | ❌ **没有**(skill.go 不注入 runctx) | skill.go:235-243 |
| 消息结构 | `[SYS(仅L1), (可选fork), USR(渲染后任务书)]` | skill.go:248 |

> `{input}` 的确切语义:= **调用方传进来的 args**。裸串→整串;JSON 含 `input` 键
> →该键值;JSON 无 `input` 键→未赋值(渲染成字面量,坑)。**不是**用户原始输入,
> **不是**模型喂的。

### C. 编排族 skill/component(engine: graph|workflow)+ steps

#### C1. `use: model` 步骤

```yaml
    steps:
      - name: regionList
        use: model
        prompt: "cap://prompt/.../v2"   # 模板(整段)
        args:
          question: "{$input}"           # 映射:键必须是模板里的占位符
```

| 项 | 传递路径 | 代码 |
|---|---|---|
| `prompt`(模板) | → **[USR] 用户消息**(无 system) | namespace.go:592 |
| **args** | **映射**,键**必须**匹配 prompt 里的 `{占位符}`(装配期严格校验) | namespace.go:367-373 |
| **`{$input}`/`{$user_id}`** | ✅ 有 | graph.go:353-354 |
| 还可引用 | `{步骤名}`(上一步输出) | graph.go:390 |

#### C2. `use: tools/...` 步骤

```yaml
      - name: dim
        use: "tools/local/dimension_config_query"
        args: '{"dimension_name_list":["region"]}'   # JSON 字面量模板
```

| 项 | 传递路径 |
|---|---|
| prompt / 消息 | 无(工具不调模型) |
| **args** | JSON **字面量模板**,运行期渲染后作工具 argsJSON;可含 `{$input}`/`{步骤名}` |

#### C3. `use: cap://component/...`(调循环族组件)

```yaml
      - name: analyze
        use: cap://component/common/analyze
        args: '{"company":"{$input}"}'
```

| 项 | 传递路径 |
|---|---|
| **args** | JSON 字面量模板,运行期渲染 → 作 argsJSON 传给目标;目标按 **B 的规则**接收 |
| **`{$input}`** | ✅ 在**这一步的 args 里**可用;传进目标后目标内部**不再有** `{$input}`——这一步正是"把 graph 的 `$input` 转成目标具名参数"的桥 |

---

## 2. 三处不一致(偶然复杂度)

| # | 不一致 | A 顶层 | B 循环族 | C-model 步骤 |
|---|---|---|---|---|
| 1 | **`{$input}` 断层** | ✅ | ❌ 无 | ✅ |
| 2 | **`prompt:` 的角色** | system(persona) | user(任务书) | user(整段) |
| 3 | **args 语义** | 无 | JSON对象→`{param}`/裸串→`{input}` | 严格映射,键须匹配占位符 |
| 4 | **prompt↔参数耦合** | — | brief 里写 `{param}` | **prompt 必须先知道参数名**才能写占位符 |

三个真正的坑:

1. **`{$input}` 在 B 缺失** → 循环组件拿不到原始输入,得靠调用方绕一圈传。
2. **`prompt:` 一词三义** → system persona / user 任务书 / user 整段,读配置要先知道在哪一族。
3. **args↔prompt 强耦合(C-model)** → prompt 作者必须先知道系统传哪些参数,改参数名要改 prompt。

---

## 3. 统一目标态(提案)

原则:**统一"词汇 + 规则",不合并"构造"**。三个构造本质不同(确定性调用 /
子 agent 循环 / 顶层 agent),该保留;要统一的是它们表达输入的方式。

### 3.1 一套配置形态(所有"喂给模型"的地方共用)

```yaml
prompt: <instruction>     # → 系统消息(指令/persona)。独立编写,不需知道参数名。
input:  <template>        # → 用户消息(可选)。{$input}/{param}/{step} 可用。
params: {...}             # 声明具名入参(可选)
args:   {name: value}     # 绑定具名入参(调用方/步骤填,可选)
```

### 3.2 四条统一规则

1. **`prompt` → 系统消息;`input` → 用户消息。** `prompt` 不再一词多义。
2. **一套变量命名空间,处处一致:** `{$input}`/`{$user_id}` 恒有;`{param}` 具名参数;
   `{step}` 上一步输出(仅 graph)。补掉 B 的 `{$input}` 断层。
3. **参数默认作为用户消息数据给模型(解耦):** `input` 省略时,params 自动渲染成
   一段带标签的用户消息(`名:值`)。prompt 里**可选**放 `{占位符}` 精确编织,
   **但不再强制**——没匹配的参数进用户消息数据块,不再报"占位符不匹配"错。
   于是 **prompt 不必知道参数名**,与 schema 解耦。
4. **必填参数没传仍 fail-fast**(契约校验保留;这与放松"占位符必须匹配"不矛盾:
   前者是调用方没履约,后者是 prompt 不必预知 schema)。

### 3.3 统一后每类长什么样

**B 循环族(解耦后):**

```yaml
- name: analyze
  engine: react
  params: {company: {type: string, required: true}}
  prompt: "你是竞品分析助手,产出结构化分析。"   # 系统指令,不含参数名
  input: |                                      # 用户消息(可选显式)
    公司:{company}
    原始问题:{$input}                           # 现在 {$input} 可用
```
或省略 `input:`,params 自动渲染成用户消息数据块。prompt 不再提 `{company}`。

**C1 model 步骤(解耦后):**

```yaml
- name: regionList
  use: model
  prompt: "cap://prompt/.../v2"   # 系统指令(外部,不需要有占位符)
  input: |
    用户问题:{$input}
    区域维度:{regionDimension}
```
不再有"键必须匹配占位符"的耦合;外部 prompt 原样复用。

**A 顶层 agent:** 基本就是目标态的参照(prompt.system=系统、输入=用户)。统一是让
B/C 向它看齐。

### 3.4 统一前后对照

| | 统一前 | 统一后 |
|---|---|---|
| `{$input}` | A✅ B❌ C✅ | 处处 ✅ |
| `prompt` 角色 | system / user / user | 一律 **system** |
| 用户消息来源 | 隐含在 prompt / brief | 显式 `input:` 或 params 自动成数据块 |
| args↔prompt | C-model 强耦合(须匹配占位符) | 解耦(占位符可选) |
| 缺必填参数 | C 打印消息 / B 静默 | 一律 fail-fast |

---

## 4. 分期迁移(按性价比)

| 阶段 | 内容 | 成本/风险 | 收益 |
|---|---|---|---|
| **P1** | 补 `{$input}`/`{$user_id}` 内置进循环族任务书,与 graph 对齐 | 低,纯新增,向后兼容 | 高——干掉最尖锐 footgun |
| **P2** | args 语义 + fail-fast 措辞统一;放松"占位符必须匹配"、未匹配参数进用户数据块 | 中 | 中高——解耦 prompt↔schema |
| **P3** | 角色统一:`prompt`=system、`input`=user,铺到 A/B/C 三处 | 高,硬切迁移(新语法嵌进错误消息当迁移指南) | 高——一套心智模型 |

**建议:** P1 先做(小、安全、直击痛点)。P2/P3 是硬切,动整个配置面——趁 1.0
冻结前值得投,但要分期、每期真机验证,不 big-bang。

---

## 附:排除项(不是配置复杂度来源)

rewoo/router/plan-execute/reflection 引擎内部自建 system/user、绕过 Modifier、
无 `{$input}`——但那是**引擎内部实现**,对 config 作者不可见(对外仍是 B 的统一
`brief.Render` 接口)。所以引擎多样性不进本文档的统一范围。
