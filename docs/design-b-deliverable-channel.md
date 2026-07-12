# 方案 B:交付物通道(Deliverable Channel)——架构不变,给交付物开确定性直达通路

> 状态:提案,与 design-a-single-agent-first.md 二选一(或分期)。
> 问题定义见方案 A 文档 §0(两方案共享)。

## 1. 设计原则

**证据与交付物分流**:证据(查询/画像/扫描)照常被大脑合成;交付物
(报表/文档/盘点产出)由 Ring 0 捕获原文、经出站事实位直达用户,大脑
只保留**策展权**(选哪些、怎么导读),失去**转述权**(不再经它的嘴)。

业界对齐:LangChain `ToolMessage.artifact`(content 给模型/artifact 给
程序)、Google A2A 的 Artifact 一等对象、Anthropic 多智能体研究系统的
"subagent 产出写文件绕过 lead"、`return_direct`——四个生态独立收敛的
同一形状。

## 2. 机制设计

### 2.1 配置词汇(能力级,+1 个键)

```yaml
skills:
  - name: sales-report
    deliver: attach          # 无(默认,证据)| attach | direct
```

- **无(默认)**:现状,结果由大脑消费合成。
- **attach**:结果原文由框架存底;终答引用即随行直达用户。
- **direct**:本轮唯一有效动作就是取该交付物时,原文直接作为终答
  (跳过最后一次模型转写);不满足触发条件退回 attach 行为。

装配期校验:`deliver` 仅 skill/component 级能力可声明(工具级证据
天然不该标),枚举 fail-fast。

### 2.2 Ring 0 捕获中间件 `DeliverResults`

位置:applyGates / agent 装配链,紧邻 DigestResults(同族机制:一个为
省上下文存原文,一个为保真交付存原文,**共用 result store**)。

行为:被标记能力返回后——
1. 原文落 result store,键 `deliver:<turn>:<seq>`,得到 id(如 `d1`);
2. 给模型的结果内容**前缀注入标记**:
   `[交付物#d1 已存底,终答引用 #d1 即原文随行,无需复述全文]` + 原文
   (原文照给——大脑仍需读它来写导读与决策;digest 对超长结果照常
   工作,消化摘要里保留 #d1 标记);
3. 交付物元信息(id/标题/能力名/长度)记入 TurnState。

### 2.3 出站事实位与"引用即附带"

- `channel.Outbound` 增字段(channel.go:46):

```go
// Deliverables 是本轮交付物原文(框架事实位):终答引用的交付物由
// 框架按 id 展开随行,呈现形态由装饰器决定(独立卡片/附件/正文追加)。
Deliverables []Deliverable   // {ID, Title, Source, Content}
```

- 收口时框架扫描终答文本中的 `#dN` 引用,从 TurnState/store 取原文
  填入 Deliverables;`attach: always` 的能力不待引用恒附带。
- `agent.Run` 签名不动:交付物经 runctx(TurnState)携带,dispatcher /
  HTTP handler 在 Run 返回后读取——与进度事件同一传递模式。

### 2.4 呈现(各通道)

| 通道 | 形态 |
|---|---|
| 飞书 | 终答卡片后逐个独立卡片(28KB 护栏内分页,复用 cardMaxBytes 逻辑) |
| CLI | 终答后 `── 交付物 #d1 <标题> ──` 分节打印 |
| HTTP | 响应体增 `deliverables` 数组 |
| 装饰器 | Outbound 事实位可见,第三方自由定制(折叠/链接/上传) |

### 2.5 direct 模式的触发与守卫

触发条件(全部满足):目标能力标 `direct`;本轮该能力恰被调用一次;
其返回后模型再无其他工具调用。满足则终答 = 交付物原文(收口在
agent.Run 层做替换);ReviewModel 守卫照过(拒绝核对等作用于替换前的
模型终答,替换只发生在守卫通过后)。不满足任一条件 → 按 attach 走。

### 2.6 未附带的交付物不丢

所有 deliver 产出都在 result store(带 TTL,复用 digest 配置),
read_result 已能按 id 分页取回——用户追问"给我看 X 的完整版"时大脑
取回再引用,下一轮随行。

## 3. 改动清单(小步,均可独立验证)

1. `capability.Meta` 增 `Deliver` 字段 + 配置解析与校验(config 层);
2. `loop.DeliverResults` 中间件 + TurnState 传递(runtime/loop 新文件,
   ~150 行);
3. `Outbound.Deliverables` + dispatcher/HTTP 收口扫描(serving,~80 行);
4. 飞书/CLI 呈现 + ops-card 装饰器示例更新(impl + examples);
5. direct 模式(agent.Run 层,~40 行);
6. L1 加一句:引用 `#dN` 即交付,豁免简洁性、不得复述全文
   (提示词变更 → MiniMax A/B:验证模型学会引用而非照抄)。

预估总量 ≤500 行 + 测试;不动引擎、不动 skill 执行语义、不动现有配置。

## 4. 代价与风险(如实)

- **模型要学会引用**:`#dN` 引用是新的行为约定,弱模型可能既引用又
  复述(冗余不失真,可接受)或不引用(退化为现状,不更糟)。A/B 验证
  是必要环节;`attach: always` 是不依赖模型行为的保底形态。
- **IM 消息变多**:一轮多附件时刷屏——§2.3 的"引用即附带"+ 装饰器
  折叠是控制面,默认不附带未引用产出。
- **不解决"合成型交付物"**:大脑跨多源亲笔撰写的内容仍可能失真
  (业界同样无解,属模型质量问题);本方案只保证"搬运型交付"零损耗。

## 5. 适用判断

方案 B 的甜区正是我们的目标画像:IM/HTTP 接入(用户看不到过程原文)、
多域并行分析(子循环隔离与并行保留)、中档模型(不依赖模型自觉搬运)。
对 CC 形态的需求(轻任务少边界),现状本就支持工具直挂 agent,不冲突。

## 6. 与方案 A 对比速览

| 维度 | A(单 agent 优先) | B(子循环 + 交付物通道) |
|---|---|---|
| 交付物保真 | 单跳,轻度损耗仍在(长文仍需 attach) | 零损耗(原文直达) |
| 上下文成本 | 高(全过程进主上下文) | 低(隔离) |
| 并行 | 退化,需保留子循环例外 | 原生 |
| 工程量 | 大(引擎级) | 小(Ring 0 中间件 + 出站字段) |
| 心智模型 | 两种形态并存 | 现有模型不变,+2 个配置词 |
| 与业界对齐 | Devin/CC 路线 | LangChain artifact / A2A Artifact / Anthropic 研究系统路线 |

## 7. 分期建议(若两案都要)

B 先行(小、快、直击痛点、不排斥 A);A 收窄为"轻 skill 的 inline
优化选项"进 roadmap,待 B 落地后按真实场景数据决定是否投入。
