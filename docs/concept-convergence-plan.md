# 概念收敛方案:skill 标准化 + component 拆解 + 配置面简化

> **状态:已完成(2026-07-17)**——五批全部落地,真机验证见
> concept-convergence-test-report.md。
> 日期:2026-07-17;前置:single-agent-mode-plan.md(已完成,mode 已移除)。
> 一句话:把 agent-kit 收敛成 **CC 同构的 Go harness**——概念表只剩
> agent / skill(业界标准)/ sub-agent(同构声明)/ delegate / tool,
> 编排下沉给 eino compose;配置词汇随概念砍掉一批,已配置在用的面
> (store 槽/模式语法/prompt 引用)保持现状,cap:// 内部身份协议不动。

## 1. 背景与动机

三个独立发现指向同一个病根:

1. **固定编排推广不动**:`steps:` 是只有 agent-kit 认的 YAML DAG 方言,
   在 Dify/n8n/eino compose 挤满的赛道上没有差异化,只有学习成本和
   锁定顾虑。而 Agent Skills 标准(SKILL.md)有现成生态,第一天就能跑。
2. **component 概念骑在物理分界线上**:同一个 `components:` 键,纯
   prompt+tools 的看得见主对话(inline),带 engine 的什么都看不见
   (fresh/fork)——"上下文看到什么"成了要靠推断规则排查的行为。
   病根是把"有无第二个大脑"这条不可逾越的分界线藏进了一个概念内部。
3. **范式引擎无人消费**:rewoo/reflection/router/plan-execute/graph/
   workflow 只有 examples 在用;行业(CC/DeepAgent/DeerFlow 2.0)全部
   收敛到单一循环形态,eino adk 自己对共享上下文编排的实证结论是
   NOT RECOMMENDED。

收敛后的分界线变成概念的名字本身:**skill = 主大脑亲自执行(主上下文
全量可见);sub-agent = 第二个大脑(必然隔离,显式传参)**。中间态不
存在,也不该存在。

## 2. 目标形态

### 2.1 概念表(收敛前 → 收敛后)

| 收敛前 | 收敛后 | 业界对应 |
|---|---|---|
| agent + profile 链(4 级) | agent(3 级:app→agent→sub-agent) | CC 主循环 |
| skill × 3 形态(steps / use / from) | **skill = Agent Skills 标准**(内联卡 或 from 包) | CC skills |
| component × 2 形态(过程卡 / 子执行体) | 过程卡并入 skill;子执行体 → **sub-agent 声明** | `.claude/agents/*.md` |
| engine × 7(react/direct/rewoo/…/graph) | 无 engine 键(同构:永远标准循环) | CC 单循环 |
| delegate | delegate(不变) | Task tool |
| steps/needs/args/output(编排 DSL) | 删除;硬流程 → 宿主 eino compose + `AsLambda` | (无;我们的企业加分项) |
| namespace(imports/exports/components/skills) | 能力清单文件(sources+skills+agents,可挂载) | — |

### 2.2 目标配置(一个完整 agent,全部键)

```yaml
# agents/shop-assistant.yaml
name: shop-assistant
description: 电商运营助手
model: { provider: minimax, config: { api_key: ${MINIMAX_API_KEY} } }
prompt:
  system: 你是电商运营助手……

sources:                        # 工具供给源(不变)
  - name: catalog
    type: http
    config: { ... }

skills:                         # 全部 = 标准形态,永远主循环执行
  - name: price-check           # 内联卡:SKILL.md 的配置层等价物
    description: 定价审查四步流程
    params: { sku: { type: string, required: true } }
    prompt: |
      1. 用 get_product 查 {sku} 的价格与成本;……
  - from: github.com/anthropics/skills/pdf@v1    # 标准技能包
    name: pdf

agents:                         # 声明式 sub-agent(同构,永远隔离)
  - name: data-analyst
    description: 大批量数据分析(中间数据大,隔离执行)
    prompt: 你是数据分析员……
    params: { question: { type: string, required: true } }
    tools: [catalog/*]
    context: fresh              # fresh(缺省)| fork(对话快照起步)
    deliver: attach             # 交付物直达(可选)
    max_rounds: 12

delegate: { enabled: true }     # 运行时动态派生(CC Task 同位)

session: { window: 20 }         # store 槽照旧:裸 type 或 cap://store/session/<name>(§5)
```

上下文心法(写进文档,一条通吃):**先把事实显式写进输入;写不进或
写不全,才 fork**。动态派生缺省 fresh(并行 × fork = token 倍增 +
无关历史污染);声明式 sub-agent 只有"任务对象就是对话本身"的才在
声明期标 fork。

### 2.3 硬流程的承接(替代 steps)

权限门禁前置、审计必落库这类硬确定性,**不用 skill/agent 表达**
(提示词是软约束),由宿主 Go 代码用 eino `compose.Graph` 写死:
能力经 `AsLambda` 变图节点,顺序由边保证,运行期没有大脑做路由。
examples 增加 `pipeline` 样例钉住正确姿势(§7 批4)。

分工:**节点内有脑(agent 自主循环),节点间无脑(eino 图)**;
agent-kit 负责把 agent 做成合格的图节点,eino 负责连线。

## 3. 砍除清单

| 项 | 位置 | 量级 |
|---|---|---|
| graph/workflow 执行器 | runtime/engine/graph.go | 627 行 |
| rewoo / plan-execute / router / reflection / direct | runtime/engine/*.go | ~700 行 |
| Step/StepArgs/steps 解析与校验 | config/namespace.go(resolveStepArgs/applyStepDefaults/validateStepsEngine 等) | ~400 行 |
| ComponentConfig + components: 配置节 | config/schema.go / namespace.go | ~200 行 |
| NamespaceSkill 的 steps/use/engine/output/step_defaults | config/schema.go | ~80 行 |
| imports/exports + cap://component 可见性规则 | config/namespace.go | ~150 行 |
| skill.Declaration 的 Engine/EngineConfig + hasSubloopKeys 推断 | skill/skill.go | ~100 行 |

净删约 **2000+ 行**,且删的全是最贵的部分(自持 DAG 执行器的挂起
位置/并发合并/步骤重试)。

**保留不动**:react 循环与全部 harness(digest/compaction/deliver/
todo/askuser/交互记忆/Ring 0)、delegate、skillpack(fetch/lock/
BuildPack/沙箱)、serving、observe、approval/budget。

## 4. 评估一:配置能否简化 —— 能,词汇砍约一半

### 4.1 删除的配置词汇(只删随概念死掉的)

`engine` `engine_config` `steps` `needs` `args` `output` `step_defaults`
`use` `export` `imports` `components` `mode`(已删)。

**明确不动的**(已配置在用,改动是纯迁移成本无功能收益):具名
`stores`/`retrievers` 实例及其 `cap://store|retriever/` 槽引用、
include/exclude 与 approval rules 的 `cap://` 模式语法、
`cap://prompt/` 引用。

### 4.2 简化的结构

- **形态推断消失**:现在"纯 prompt+tools = 过程卡 / 带子循环键 =
  子执行体"靠 hasSubloopKeys 推断——因为一个概念装了两种形态。拆成
  `skills:` 和 `agents:` 两节后,**结构即语义,零推断**,配置作者
  不需要理解任何规则。
- **profile 链 4 级 → 3 级**:app → agent →(挂载文件缺省)→
  sub-agent;component 层消失。
- **store/retriever 具名实例保持现状**:`cap://store/<kind>/<name>`
  槽引用已在生产配置里铺开(session/todo/result/suspend/…),间接层
  承载着"一处声明、多模块共享同一后端"的真实价值,改动是纯迁移
  成本——不动。
- **namespace 减负为"能力清单文件"**:只剩 sources+skills+agents+
  profile 缺省,挂载即合并;imports/exports 随 component 死(它们
  只治理 component 可见性),跨文件同名冲突装配期直接报错。

### 4.3 简化后的心智模型

写配置只需回答三个问题:**用什么工具(sources)、会什么套路
(skills)、要不要分身(agents/delegate)**。每个问题一个配置节,
每节的上下文语义望文生义。

## 5. 评估二:cap:// 协议还需要吗 —— 内部保留,配置面退出

cap:// 有两副面孔,命运不同:

### 5.1 内部身份协议(Ref)——**保留,这是目录的脊柱**

`Ref{Kind,Domain,Name,Version}` 是全库能力的唯一身份:目录索引与
冲突检测(Key)、include/exclude 与 approval 规则的模式匹配(Match
通配)、observe 的能力命名、版本共存。它对配置作者不可见、零学习
成本,删它等于重造一个一样的东西。**不动。**

### 5.2 逐 kind 盘点(源码核实,2026-07-17)

关键澄清:**工具挂载从来不写 cap://**——`tools:` 列表用短形态
`tools/<source>/<name>`,装配层(toolPattern)内部翻译成
`cap://tool/...` 模式去目录选品。配置面出现全称 URI 的只有五处:
跨 ns 引用、include/exclude、approval rules、prompt 引用、store/
retriever 槽。逐 kind:

| kind | 内部生产者(身份) | 今天配置面何处写 `cap://<kind>/` | 收敛后 |
|---|---|---|---|
| **tool** | 全部 sources(mcp/exec/http/rpc/local/vector)+ builtins(ask_user/pack_read/model_step/…),15 处构造 | 不直接写——`tools:` 用短形态 `tools/<source>/<name>`;include/exclude、approval rules 写模式(`cap://tool/fs/*`) | **全部保留**(模式语法已配置在用,不折腾) |
| **skill** | skill.Build / pack.go(Domain=ns) | ① steps/tools 跨 ns 引用;② include/exclude;③ approval rules | ① 随 steps 死,共享=挂载即可见;②③ **保留** |
| **component** | namespace.go export(2 处) | `cap://component/<ns>/<name>`(imports 后可引) | **kind 整个删除**(随概念死) |
| **agent** | impl/source/a2a(远程 A2A agent) | 不写(经 sources 声明、短形态挂载);模式面可匹配 | 保留;**建议:sub-agent 声明的装配产物改挂 Kind:"agent"**(与 A2A 统一,身份语义归位) |
| **prompt** | 无目录身份(protocol/prompt 独立 resolver,RefPrefix) | prompt 标量字段 `"cap://prompt/<source>/<name>@label"` | **保留** |
| **store** | 无——纯配置寻址语法(resolveStoreRef) | 7 个模块的 store 槽 | **保留**(生产已铺开,共享后端的间接层有真实价值) |
| **retriever** | 无——纯配置寻址(resolveRetrieverRef) | session.recall.retriever 槽 | **保留** |
| (model) | 仅 observe 事件标签 CapKind:"model",非目录 Ref | 不出现 | 不变(观测命名) |

取舍原则:**随概念死掉的删(component、steps 里的跨 ns skill 引用),
已配置在用的不折腾**(store/retriever 槽、include/exclude、approval
模式、prompt 引用)。改语法是纯迁移成本,没有功能收益。

结果:内部身份 kind 剩 **tool / skill / agent**;配置面的 cap://
收敛为四个既有用法(store/retriever 槽、两个模式面、prompt 引用),
全部维持现状语法;推广主路径(sources/skills/agents 三节)不出现
cap://。

## 6. 跨 namespace 暴露(开放问题,随批3 定)

skill 收敛成标准包后,`cap://skill` 跨域引用消失。新的共享单位:

- **skill 包**:天然共享单位(from 同一个包);
- **sub-agent**:挂载文件里声明的 agents 对挂载方 agent 可见,
  即"文件即共享边界",不再有 export 开关。

倾向后者(挂载即可见,同名报错),实现最简;若 insight-agent 有
更复杂的可见性诉求再议。

## 7. 分批实施(每批可独立提交,全程硬切)

### 批1 引擎塌缩
删 graph/planexecute/rewoo/router/reflection/direct 及注册;engine
注册表内化(仅 react,配置面无 engine 键);`engine:` 误写报错:
`"engine has been removed: skills run on the host loop, sub-agents
always run the standard loop (declare them under agents:)"`。
steps 族配置键同批删除,误写报错指路 eino compose 样例文档。

### 批2 概念拆分
- `ComponentConfig` → `AgentDecl`(agents: 节:name/description/
  prompt/params/tools/model/context/deliver/todo/max_rounds/
  compaction),装配产物 = 现子执行体(同构循环,挂成工具);
- `components:` 误写报错指路 agents:/skills:;
- SkillEntry 收敛:内联卡(name+description+params+prompt)或
  from 包,其余键删;skill.Declaration 删 Engine/EngineConfig,
  hasSubloopKeys 推断死掉(结构即语义);
- L1 文案里"skill/component"词汇统一为"skill/sub-agent"
  (调用方契约、过程卡纪律原文保留)。

### 批3 配置面简化
- namespace 减负(删 imports/exports/components,可见性=挂载即可见,
  同名装配期报错);
- store/retriever 具名实例、include/exclude 与 approval 模式语法、
  `cap://prompt/` **均不动**(已配置在用);
- 严格 YAML 全量校验过一遍(未知键零容忍现状保持)。

### 批4 examples 迁移 + pipeline 样例
- interactive 6 个 namespace:steps skill → 内联卡或删除;范式引擎
  component → sub-agent 或内联卡;smoke 同步;
- 新增 `examples/pipeline`:三个 sub-agent 经 compose 串联 + 一个
  并行汇合变体(compose+AsLambda,替代被删的 steps 文档);
- README/docs 定位收窄:"eino 之上、原生支持 Agent Skills 标准的
  Go agent harness"。

### 批5 回归验证(家规:行为变更必须真机)
- 全量 go test + -race(skill/loop/config)+ layering;
- 旗舰交互场景真机回归(interactive 12 场景抽 4:含 skill 内联卡、
  sub-agent 隔离、delegate、deliver);
- fidelity A/B 复跑(deliverable 通道回归,确认拆解未伤直达链路);
- inline A/B 复跑(过程卡完成率/贯彻率基线不回退);
- 出测试报告 docs/concept-convergence-test-report.md。

## 8. 先决条件与风险

| 项 | 处置 |
|---|---|
| **insight-agent 是否在用 steps/components**(唯一未知数) | 动手前确认;在用则先给迁移窗口(文档标注维护态),不在用直接切 |
| examples 迁移量(6+ 文件重写) | 批4 集中做,验收 = app.yaml 严格解析 + 真机回归 |
| 范式引擎删除后的"planner 型"诉求 | todo_write + 过程卡已覆盖(单 agent 模式 A/B 数据);真缺再以 skill 形态回来,不回引擎轴 |
| 交互式硬流程(图管道里没有"用户"可问) | 文档写明边界:硬管道适合无人值守链路,交互场景走主循环 + sub-agent |
| sub-skills(标准包子目录) | 独立增强线,不进本方案(登记在案) |

## 9. 不做什么

- 不做 steps 的任何兼容层/双轨(pre-1.0 家规硬切,错误文案指路);
- 不给 sub-agent 保留 engine 选项("需要不同引擎"=需求未被证实,
  eino adk 与三方收敛均反对);
- 不动 Ref 内部协议、不动 harness 主线、不动 delegate 语义。
