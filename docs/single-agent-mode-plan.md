# 单 agent 优先执行模式(CC 形态)详细方案

> 状态:已实施(批1-6)+ **缺省已切换**:纯 prompt+tools 声明缺省 inline
> (主循环执行),带任何子循环专属键(engine/model/deliver/todo/compaction/
> max_rounds/context/engine_config)即推断 subloop——CC 语义,零配置破坏。数据:single-agent-mode-test-report.md。演进自 design-a-single-agent-first.md(草案),两处
> 关键修正见 §1.2;与已上线的交付物直达通道(deliverable-channel-plan.md)
> 的交互见 §5。参照:Claude Code 的 Skill/Task 机制、Cognition
> "Don't Build Multi-Agents"、Anthropic 多智能体研究系统复盘。

## 0. 目标形态一句话

**默认单 agent 一个上下文干到底;只在四种信号下扩展子 agent**:
①并行提效(多分支同时推进)②上下文隔离(中间数据会污染/顶爆主上下文)
③专属模型 ④治理边界(独立预算/审批面)。扩展方式两种:模型运行期
自主委派(动态)、配置声明的结构化组件(静态)。

## 1. 核心机制

### 1.1 Claude Code 的机制对照(以终为始)

| CC 机制 | 本质 | agent-kit 对应物 |
|---|---|---|
| Skill(SKILL.md) | **指令注入**:skill 内容作为工具结果进主上下文,主模型照着做;工具**常驻挂载**,allowed-tools 是权限闸不是可见性闸 | 过程卡(§1.3) |
| Task/subagent | 模型运行期写 prompt 委派一个子循环,子上下文隔离,只回终答 | 动态委派 delegate(§1.4) |
| 文件系统 | 交付物通道(聊天只给导读) | 交付物直达通道(已上线) |
| compaction | 单上下文的生命线 | 已有,需配套加强(§4.3) |

### 1.2 对草案 A 的两处修正

1. **砍掉"轮内工具面解锁"**:草案 A 设计了 use_skill 展开后动态解锁工具
   子集,需要 eino react 轮内重绑工具面——工程量最大的一块。CC 的真实
   做法是**工具常驻挂载**,靠系统提示词的能力路由纪律管选择。修正后
   本方案零引擎改动。工具面膨胀的治理见 §4.4。
2. **补上动态委派**:草案 A 只保留了配置声明的子形态,漏了 CC 模式的
   另一半——Task 式运行期委派(模型现场决定"这活儿该隔离跑")。这是
   "必要的时候扩展子 agent"的主通道,②类信号(上下文污染)只有模型
   在现场才判断得准。

### 1.3 过程卡(skill 的 inline 执行形态)

**配置**(skill/component 级 +1 键,缺省不变):

```yaml
skills:
  - name: price-review
    mode: inline            # inline | subloop(缺省,现状)
    prompt: "cap://prompt/local/price-review@v3"
    tools: ["tools/shop/get_product", "tools/shop/sales_summary"]
```

**装配语义**(config/namespace.go + skill/skill.go:119 分支):

- 产物仍是一个 capability(目录/选品/跨 ns 引用全部不变),但 handler
  从"跑子循环"换成"**渲染任务书并返回**":调用即把 brief.Render(args)
  的结果(skill 的指令文本)作为工具结果注入主上下文,主模型接着照做;
- 该 skill 声明的 `tools` **同时直挂到宿主 agent 工具面**(常驻,CC 语义;
  跨 ns 撞名经既有 Rename 前缀化,capability.go:217);
- skillpack(SKILL.md)天然适配:它本来就是写给模型读的指令文档,
  `mode: inline` 下 BuildPack 跳过子循环装配,返回文档本体 + 直挂
  allowed-tools 白名单内的工具;
- **约束**:inline 与 engine 互斥(过程卡没有引擎;rewoo/graph 等结构化
  形态就是该用 subloop 的场景),与 `deliver:` 互斥(见 §5),违约装配期
  报错指路。

**Meta 约定**:过程卡 Risk=Readonly(返回指令无副作用;真实副作用在
被直挂的工具上,各带各的审批闸)、描述追加统一后缀"(调用返回执行
指引,按指引使用工具完成)",防模型把过程卡当成"调用即完成"。

### 1.4 动态委派(builtin `delegate`)

**新内置工具**(与 ask_user/todo 同级,默认关、配置开启):

```yaml
agents:
  - name: ops-manager
    delegate:
      enabled: true
      max_rounds: 8          # 子循环轮数缺省(可被调用参数收紧,不可放宽)
      max_parallel: 4        # 同轮委派并发上限
```

**工具契约**(模型可见):

```
delegate(task: string, tools?: []string, context?: "fresh"|"fork")
  把一个可独立完成的子任务委派给隔离执行体:适合并行推进多条线索、
  或中间数据量大不宜进入当前对话的任务。子任务只返回最终结果,
  过程不进入你的上下文。tools 缺省 = 你当前的工具面(去掉 delegate
  自身);context: fork 携带当前对话快照。
```

**执行语义**(runtime 侧,复用既有件):

- 实现 = 现场组装一个 react 子循环:模型 = 宿主模型、工具 = 按 tools
  白名单从宿主面选品、L1 = 子循环变体(含受托执行契约)、Ring 0 全套
  (审批/预算/digest/dedup 经 ctx 穿透,与 component 子循环同一纪律);
  本质是 skill.Build 的运行期版本,装配代码复用度 ~80%;
- **并行**:模型同轮发多个 delegate 调用 → react 引擎的批并行天然生效,
  max_parallel 之外的排队执行;
- **治理**:深度 1(子循环工具面不含 delegate,防递归裂变);轮数上限
  只可收紧;预算/审批经 ctx 与宿主共享账本;scope 记 `dlg:<hash>#N`,
  dedup/进度/轨迹按既有机制隔离;
- **进度**:委派子循环的工具调用经 ProgressTools 上报,IM 卡片可见
  "⚙ delegate(扫描存储品类)… ⚙ get_product…"层级。

### 1.5 保留的静态子形态(不动)

components(循环族/编排族)、graph/rewoo 的结构化并行、跨 agent 的
cap://skill 委托、agent-as-capability——全部保留原语义。它们是"设计期
就知道要隔离/并行"的形态;delegate 覆盖"运行期才知道"的场景。

### 1.6 决策框架(写进 L1 与文档)

```
默认:主循环直接干(工具就在手边,不建边界)
需要照着领域 SOP 干        → 调过程卡(inline skill),按指引继续
多条独立线索可同时推进      → delegate 并行(或设计期已知则 graph)
中间数据大/脏、会污染上下文 → delegate(context: fresh)
固定流程、确定性步骤        → workflow/graph(编排,不烧脑)
需要不同模型/独立治理面     → component(subloop)
```

## 2. 与现状的行为差异(用户视角)

| 场景 | 现状(subloop) | inline 模式 |
|---|---|---|
| price-review 单品审查 | 子循环 4-6 次模型调用,终答转写 | 主循环 2-3 次调用,做工作的模型直接写答案,**少一跳转述** |
| 全店扫描(6 品类) | 6 个 bulk-audit 子循环并行 | 模型 delegate 6 个并行子任务,**或**主循环串行(模型自选) |
| token 成本 | 子循环历史用完即弃 | 中间结果驻留主上下文,**压缩层压力上升**(§4.3 配套) |
| 交付物保真 | attach 机制零损耗 | 模型亲笔写 = 合成型,机制保证不可用(§5) |

## 3. 收益与代价(如实)

**收益**:转述损耗从两跳减到一跳;跨工具推理不再隔着 skill 边界
(现状:主循环拿不到子循环的中间观察);配置心智更简单(轻 skill
不再需要理解引擎);token 总量在中小任务上更低(免去子循环的 L1/
brief 重复注入)。

**代价**:①主上下文膨胀,长任务的 compaction 摘要损耗上升——中间数据
重的任务 inline 反而更差,这正是 delegate 存在的理由;②工具面变大
(interactive 全 inline 化 ≈ 30+ 工具),中档模型选择性下降,需 §4.4
治理 + A/B 实证;③skill 行为质量与宿主上下文状态耦合(黑盒契约减弱),
跨 agent 复用的确定性下降;④外部 skillpack 的指令直接进主上下文,
注入面扩大,需信任分级(§4.6)。

## 4. 配套升级清单(同步改,缺一则形态跛脚)

| # | 配套 | 内容 | 必要性 |
|---|---|---|---|
| 4.1 | **L1 提示词** | 新增两节:过程卡纪律(收到指引即按步执行,完成前不转做他事,todo 跟踪多步指引)+ 委派决策框架(§1.6 的模型可读版) | 硬依赖:没有纪律,过程卡指令会被"知道了"一句带过 |
| 4.2 | **todo 默认策略** | inline 主循环的长任务没有子循环轮数护栏,todo 从"可选"升为 L1 强引导(≥3 步指引必须 todo 跟踪);现有 todo 契约行已适配 | 硬依赖:单上下文长跑的失焦防线 |
| 4.3 | **上下文管线** | ①digest.degrade_keep(存储降级时提高保留量,前议题);②tool_clear 默认从 800 收紧评估(中间结果更多);③compaction 增量摘要在 40+ 工具调用场景的真机压测;④record_tools=summary 在 inline 下的会话膨胀评估 | 硬依赖:inline 的生命线 |
| 4.4 | **工具面治理** | ①描述瘦身审计(30 工具 × 平均 80 token 描述 = 2400 token 常驻);②领域前缀命名规范(shop_/order_/crm_);③A/B:工具数 10/20/30 阶梯下 MiniMax 的选择准确率(eval 套件新尺子) | 硬依赖:中档模型的可用性前提 |
| 4.5 | **进度/观测** | delegate 子循环的 scope 层级进 ProgressEvent(卡片可折叠);轨迹记录按 dlg: scope 分组 | 重要:委派不可见 = 排障黑洞 |
| 4.6 | **skillpack 信任分级** | 外部包 inline 需显式 `trust: inline`(缺省只允许 subloop):外部指令进主上下文 = 完整指令权,必须是显式决定 | 硬依赖:安全边界 |
| 4.7 | **配置校验** | mode 枚举 fail-fast;inline×engine/deliver 互斥;delegate.enabled 与 max_* 校验;ns 工具直挂的撞名报告 | 常规纪律 |
| 4.8 | **eval 扩展** | ①inline vs subloop 同场景对照(质量/token/时延三指标,interactive 的 12 场景复用);②上下文压力场景(40+ 调用);③工具面阶梯(4.4);④委派决策质量(该委派时委派了吗) | 硬依赖:形态切换必须有数据 |
| 4.9 | **文档/示例** | README 执行形态章重写(三形态决策框架);interactive 迁移 3-4 个轻 skill 作 inline 样板;CHANGELOG Breaking 说明 | 常规 |

## 5. 与交付物直达通道的交互(重要且诚实)

- **inline 与 `deliver:` 互斥**:通道保真的前提是"子循环终答 = 完整交付
  物原文",inline 下没有这个终答——报表由主模型亲笔写,是**合成型**
  交付,机制性零损耗**不可用**,回到模型质量。这是两形态的本质权衡:
  **inline 消除证据转述损耗,subloop+attach 消除交付物转写损耗**;
- 所以推荐配置形态:**证据类轻 skill → inline;交付物类 skill →
  subloop + deliver: attach**。两者在同一个 agent 里共存,按能力语义
  各选各的——这也是本方案不做全局硬切、只做能力级 mode 的根本原因;
- 数据工具的 deliver 标记(工具产出本身是交付物,如导出类)不受影响。

## 6. 分批实施

| 批 | 内容 | 出口验证 |
|---|---|---|
| **批1 过程卡** | skill.Build mode 分支 + ns 装配直挂 tools + 撞名处理 + 互斥校验 + Meta 约定 | 单测(装配形态/校验)+ interactive 迁 quick-product-qa 真机对照 |
| **批2 L1+todo** | 4.1 两节 + 4.2 强引导 | MiniMax A/B:过程卡多步指引完成率(n≥6,对照无纪律臂) |
| **批3 delegate** | builtin + 运行期子循环组装 + 并行/深度/轮数治理 + scope/进度 | 单测(治理边界)+ 真机:全店扫描场景模型自主委派观察 |
| **批4 上下文配套** | 4.3 全项(degrade_keep 并入)| 压力场景真机(40+ 调用)+ compaction 指标 |
| **批5 工具面+信任** | 4.4 审计与规范 + 4.6 trust 键 | 工具面阶梯 A/B 数据 |
| **批6 eval+迁移** | 4.8 四组尺子 + interactive 样板迁移 + 4.9 文档 | inline vs subloop 对照报告(决定是否扩大迁移范围) |

预估:批1/3 各 ~300 行,批2/4/5 各 ~100 行 + 测试;全程 subloop 语义
零变化,存量配置零迁移(mode 缺省 subloop)。

## 7. 风险与回退

- **最大风险在批2/4 的模型行为**(过程卡纪律、压缩质量),不在代码——
  所以 A/B 前置在批2,数据不过关(完成率 < subloop 基线的 90%)则 inline
  收窄为"≤2 工具的微 skill"专用,不推默认;
- 每批独立开关:mode 缺省 subloop、delegate 缺省关——**任何时点回退 =
  改回配置**,无数据迁移、无行为残留;
- 明确不做:全局默认切 inline(等批6 数据)、嵌套 delegate、subloop
  语义任何改动。
