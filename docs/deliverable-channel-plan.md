# 交付物直达通道(Deliverable Channel)实施方案

> 状态:定稿待执行。前置讨论见 design-b-deliverable-channel.md(方案对比)
> 与 design-a-single-agent-first.md(被搁置的替代形态,收窄进 roadmap)。
> 目标:skill/component 的交付物原文零损耗直达用户;大脑保留策展权
> (选哪些、写导读),失去转述权(原文不经它的嘴)。

## 0. 不做什么(边界先立)

- **不解决合成保真**:大脑跨多源亲笔撰写的内容失真属模型质量问题,
  业界同样无解;本方案只保证"搬运型交付"零损耗。
- **不做跨轮引用**(v1):终答只能引用本轮产生的 #dN;跨轮取历史交付物
  走既有 read_result(取回后本轮再引用即随行)。
- **不改 agent.Run/Stream 签名**:交付物经 ctx 注入的收集器传递
  (与 Interactor 同一注入模式)。
- **不动引擎**:全部改动在 Ring 0 中间件、装配、出站三层。

## 1. 词汇与数据结构

### 1.1 配置面(+1 键,能力级)

```yaml
skills:
  - name: sales-report
    deliver: attach        # 无(默认=证据)| attach | always | direct
```

- `attach`:存底;终答引用 `#dN` 即随行。
- `always`:存底;不待引用恒随行(合规记录类;显式选择刷屏)。
- `direct`:满足触发条件时原文直接作为终答;否则退化为 attach。
- 声明位置:平铺 SkillEntry、NamespaceSkill、ComponentConfig
  (循环族与编排族均可);**工具级(sources 里的 tool)不可声明**,
  装配期报错——工具结果天然是证据。枚举 fail-fast(对齐 P1-B 纪律)。

### 1.2 core 层(跨层词汇,对齐 TagRawResult 先例)

```go
// core/capability/capability.go
type DeliverMode string
const (
    DeliverNone   DeliverMode = ""        // 证据(默认)
    DeliverAttach DeliverMode = "attach"
    DeliverAlways DeliverMode = "always"
    DeliverDirect DeliverMode = "direct"
)
// Meta 增字段
Deliver DeliverMode
```

```go
// core/runctx(与 Interactor 同层的注入原语)
type Deliverable struct {
    ID      string // d1, d2...(轮内序)
    Title   string // 能力名 + 首行标题启发
    Source  string // cap://... 完整引用
    Mode    capability.DeliverMode
    Content string
}
func WithDeliverableSink(ctx) (context.Context, *DeliverableSink)
func EmitDeliverable(ctx, d Deliverable)      // sink 缺席时 no-op
```

Sink 并发安全(同轮并行工具);ID 由 sink 原子分配保证唯一有序。

## 2. Ring 0 捕获中间件

### 2.1 `loop.DeliverResults(caps, kv, ttl)`

新文件 runtime/loop/deliver.go。行为(仅包装 Meta.Deliver != "" 的能力):

1. inner 返回后:原文经 EmitDeliverable 进 sink;同时落 KV
   (键 `deliver\x1f<scope>\x1f<id>`,TTL 同 result;后端复用 ResultKV
   ——两者同族:一个为省上下文存原文,一个为保真交付存原文);
2. 给模型的结果**前缀注入标记**(框架统一注入,非 skill 自述):
   `[交付物#d1|<能力名>] 已存底;终答引用 #d1 即原文随行,无需复述全文。\n` + 原文;
3. KV 写失败:降级为纯 attach-in-memory(sink 仍有,本轮随行不受影响,
   跨轮取回不可用),Warn 一条——**交付通路不因存储故障拉闸**
  (对齐挂起 fail-open 的教训)。

### 2.2 链位(两处装配点同序)

```
TimeoutTools → DedupCalls → **DeliverResults** → DigestResults → Truncate → …
```

- 在 Digest **内侧**:捕获的是未消化原文;
- 在 Dedup **外侧**……不,在 Dedup 内侧:重复调用被拦截回放时 inner
  不执行 → 不重复捕获(回放文本自带上次的 #dN 标记,引用仍有效)。
- 落点:config/agent.go:295 区与 skill/skill.go applyGates,一行插入。

### 2.3 digest 交互(一条豁免规则)

DigestResults 消化超长结果时,**保留首行 `[交付物#dN|…]` 标记原样进
摘要头部**(digest.go 摘要拼装处 +3 行)——消化丢的是模型上下文里的
细节,交付物原文在 KV 里完好,引用链路不断。

## 3. 出站:引用扫描与事实位

### 3.1 Outbound 增字段(protocol/channel/channel.go:46)

```go
// Deliverables 是本轮交付物原文(框架事实位):终答引用的交付物由
// 框架按 id 展开随行;呈现形态由装饰器/适配器决定。
Deliverables []channel.Deliverable   // 与 runctx.Deliverable 同形
```

### 3.2 收口扫描(serving 层公共助手)

```go
// serving/deliver.go
func resolveDeliverables(answer string, sink *runctx.DeliverableSink) []channel.Deliverable
```

规则:
- 正则 `#d\d+` 扫终答,命中的按**引用出现序**收集(同一 id 只收一次);
- mode=always 的未引用也追加(排在被引用者之后);
- 引用了不存在的 id:忽略 + Warn(模型幻觉引用不炸轮);
- **护栏**:单轮上限 5 个 / 总 200KB(常量,注释说明),超限按序保留、
  截断处 Warn——防模型全量引用绕过策展。

### 3.3 注入点(三处)

| 入口 | 改动 |
|---|---|
| IM dispatcher(serving/dispatcher.go job 执行) | Run 前 WithDeliverableSink;终答 Outbound 填 Deliverables |
| HTTP /messages(serving/serving.go:129 区) | 同上;响应体增 `deliverables` 数组 |
| 裸 ag.Run(examples CLI) | main.go 装 sink,答案后分节打印 |

## 4. 呈现

- **飞书**(impl/channel/feishu):Send 收到带 Deliverables 的 Outbound
  时,终答后逐个发独立卡片;单个超 28KB 复用 cardMaxBytes 分页为多卡
  (标题带 `(1/3)`)。适配器不认识 Deliverables 的通道(自定义 channel)
  自动忽略——字段是增量事实,零值兼容。
- **ops-card 装饰器**(examples/interactive/card.go):交付物卡片加
  标题头(来源能力名)+ 折叠面板可选,作为第三方定制参考。
- **CLI**:`── 交付物 #d1 <标题>(sales-report)──` 分节打印。

## 5. direct 模式

触发条件(全部满足,agent.Run 收口处判定):
1. sink 中恰有一个 mode=direct 条目;
2. 该条目是本轮**最后一次**工具调用(sink 记全局调用序,TurnState 计数);
3. ReviewModel 全部守卫已通过(拒绝核对/收口检查作用于模型原终答)。

满足 → 答案替换为交付物原文(模型终答留轨迹不外发);不满足 → 该条目
按 attach 处理。会话落痕存**替换后的**答案(用户看到什么,历史记什么)。

## 6. 提示词(L1,一句)

loopPromptTail 增:

> When a tool result carries a deliverable marker like [交付物#d1|...],
> reference #d1 in your final message to deliver it verbatim — the full
> content travels with your answer automatically. Do not restate its body;
> deliverable references are exempt from the conciseness rule.

行为变更 → 按家规 MiniMax 真机 A/B(见 §7 批 4)。

## 7. 分批实施(每批独立可验证、可提交)

| 批 | 内容 | 验证 |
|---|---|---|
| **批1 词汇+捕获** | Meta.Deliver、config 解析/校验(三处声明位+工具级拒收)、runctx sink、DeliverResults、digest 标记豁免 | 单测:捕获/标记/并发 id/KV 失败降级/dedup 回放不重捕;config 枚举 fail-fast 用例 |
| **批2 出站** | Outbound.Deliverables、resolveDeliverables、dispatcher/HTTP/CLI 三注入点 | 单测:引用序/always 追加/幻觉 id/护栏截断;serving e2e(fakeChannel 断言附件) |
| **批3 呈现** | 飞书独立卡片+分页、ops-card 示例、CLI 分节 | 飞书真机手测(体检脚本扩展);28KB 分页单测 |
| **批4 direct+提示词** | direct 判定与替换、L1 语句 | MiniMax A/B n≥6:标 attach 的 sales-report,量"引用率/复述率/关键列保留";direct 单测+live 一条 |
| **批5 尺子+文档** | eval 交付物保真尺子(列/行/章节保留率,进 eval-suite);README 输入模型章补 deliver、config-taxonomy 词条、CHANGELOG | 现有 interactive 场景回归(S2/S8b 重跑对照附件完整性) |

预算:批1-2 各 ~200 行,批3-4 各 ~150 行,批5 文档+用例;全程不动
引擎与既有配置语义,老配置零迁移(deliver 缺省即现状)。

## 8. 验收标准

1. interactive:「音频出 30 天销售报表」→ 飞书收到导读卡片 + 报表原文
   卡片,原文与子循环产出逐字节一致;
2. 全店扫描(6×bulk-audit 标 attach)→ 零附件(未引用);追问单份 →
   read_result 取回后随行;
3. ResultKV 断连时 → 本轮附件照常、Warn 一条、消息通路无影响;
4. A/B:引用率 ≥ 80%(MiniMax),复述全文率显著下降,关键列保留率 100%
  (附件侧恒 100% 是机制保证,量的是终答侧行为)。
