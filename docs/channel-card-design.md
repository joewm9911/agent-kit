# IM 通道扩展方案 v3:执行进度流 + 单一装饰点

> 状态:设计稿 v3(2026-07)。演进:v1 模板渲染为中心 → v2 两级装饰链
> → v3 收敛为**单一装饰点,框架不做任何通道样式定制**。立场:**框架给
> 机制与事实(事件流 + 装饰点 + 语义字段),呈现策略 100% 归第三方**。

## 0. 两个诉求与设计立场

1. **底层支持 processing 过程流,第三方可订阅**,如何呈现(更新卡片/
   发进度条/写日志/推别处)由订阅方决定;
2. **出站消息发送前支持第三方装饰**——从改文案到整卡结构,一个装饰点
   全覆盖;框架自身不针对飞书定制 header/模板/表格,这些全是第三方
   装饰器的事。

立场:两者都是**机制 vs 策略**的切分。框架保证事件如实产生、装饰点
如实存在、语义事实如实填充(Ring 0,代码保证);产生什么样的用户体验
是策略,归第三方。

## 1. 总体架构

```
                    ┌────────────── 执行域(runtime)──────────────┐
 用户消息 ─▶ dispatcher ─▶ agent.Run ─▶ engine ─▶ 模型/工具/技能
                    │                        │
                    │              eino callbacks 切面(observe)
                    │                        │ 结构化进度事件
                    │                        ▼
                    │              runctx.ProgressSink(ctx 注入)
                    │                        │
                    │        ┌───────────────┼────────────────┐
                    │   内置订阅者        第三方订阅者      (登记:SSE 透出)
                    │ (Progress 填进更新) (Binding.OnProgress)
                    │
                    └─▶ Outbound{Kind, Text, Progress, Meta}
                                    │
                          装饰点 Binding.Decorator(第三方,可选)
                            ├─ 改语义字段 → 适配器默认渲染(最简卡片)
                            └─ 构造 Native → 适配器原样透传(整卡自定义)
                                    │
                              channel.Send/Update ─▶ 飞书 OpenAPI
```

## 2. 执行进度流

### 2.1 归属决策:不放 channel,新起一小套

进度事件是**执行域**的事实(工具开始/结束、技能子循环、评审重试),
不是 IM 概念——CLI 进度行、HTTP SSE、轨迹落盘消费的是同一股流。放进
channel 契约会把每个 IM 适配器都耦合上执行语义,还把非 IM 消费者排除
在外。按分层归位:

| 层 | 职责 |
|---|---|
| core/runctx | 定义事件与接收器类型(零依赖,谁都能 import) |
| runtime/observe | 发射:eino callbacks 切面已看见每次模型/工具调用,新增一个 handler 把它们转成结构化事件、发给 ctx 里的 sink(engine 零改动) |
| serving | 订阅接线:dispatcher 在 card 模式装内置订阅者;Binding 暴露第三方订阅点 |
| 第三方 | 任意消费:自己更新卡片、推外部系统、忽略 |

### 2.2 事件模型(core/runctx)

```go
// ProgressEvent 是一次执行步骤的进度事实(结构化,非展示文案)。
type ProgressEvent struct {
    Seq    int           // 轮内序号,发射侧单调递增——被丢弃的事件留下
                         // 可检测的序号缺口,订阅方据此感知有损
    Kind   string        // tool | skill | model
    Name   string        // 能力名/模型名
    Status string        // start | done | error
    Dur    time.Duration // done/error 时的耗时
    Detail string        // 参数摘要 / 错误摘要(截断,防大 payload)
}

// ProgressSink 是订阅者回调,由框架的投递 worker 异步调用——
// 订阅者的耗时/阻塞/panic 都影响不到执行主流程。
type ProgressSink func(ctx context.Context, ev ProgressEvent)

// WithProgress 安装订阅:内部创建有界队列(默认 64)+ 投递 goroutine
// (生命周期随 ctx,ctx 结束即退出)。发射侧对队列做非阻塞写,队列满
// 直接丢弃并计数——主流程在任何情况下都不等待订阅者。
func WithProgress(ctx context.Context, sink ProgressSink) context.Context

// emit 是发射点内部入口:无订阅时零开销(判 nil 即返回)。
```

要点:
- **事件是事实不是文案**:「⚙ 调用 X」还是「Step 2/5」是订阅方的事;
- **不装 sink 零开销**:发射点判 nil 即返回,现有路径不变;
- **发射侧永不阻塞(结构性保证,非契约)**:异步有界队列 + 非阻塞写。
  同步回调曾是 v2 初稿方案,被否——"订阅者必须快"是靠文档约束自觉,
  违反"纪律靠 harness"原则:订阅者里一次 IM 网络调用就拖慢每步工具
  执行,挂住就 hang 整个主循环。队列/goroutine 的生命周期问题用
  ctx 绑定解决(轮次结束 worker 退出),不外泄管理负担;
- **丢弃语义**:进度是有损可接受的提示性信号(UI 刷新),队列满丢新
  事件 + 计数;Seq 缺口让订阅方可感知丢弃;**终稿收口不走这条流**
  (dispatcher 的 answer 更新是主路径),丢进度不丢结果。

### 2.3 发射点(runtime/observe)

复用既有 callbacks 切面(observe.Progress 已在同一位置做文本进度行):
新增 `observe.ProgressEvents()` handler,App 装配期挂一次,运行期从
ctx 取 sink 发射。v1 只发射 tool/skill/model 三档(与现有进度行同粒
度);评审重试、compaction 事件登记为扩展(同一事件模型,加 Kind 即可)。

### 2.4 订阅面(三层,由近及远)

| 订阅方式 | 形态 | 适用 |
|---|---|---|
| 内置订阅者 | dispatcher card 模式自动装:节流 ≥2s,把过程行写进占位卡的折叠面板 | 默认体验,零配置 |
| Binding.OnProgress | `func(ctx, conv ConvRef, ev ProgressEvent)`,装了它内置订阅者让位 | 第三方完全接管 IM 呈现(自己 Send/Update,想画什么画什么) |
| 通用 ctx 注入 | 任何直接调 agent.Run 的宿主自己 `runctx.WithProgress` | CLI、HTTP 服务、测试 |

登记不做:SSE/webhook 向进程外推流(出现真实诉求再开,事件模型不变)。

### 2.5 接线方式:函数值 vs 按名注册

OnProgress/Decorator 是 Go 函数,YAML 表达不了。两条接线路径:

**嵌入方**(自己写 main 直接装配)——直接塞函数值:

```go
serving.Binding{Channel: ch, Agent: ag, ReplyMode: "card",
    OnProgress: myHandler, Decorator: myCard}
```

**配置方**(经 config.Build 从 YAML 装配)——按名注册表(init 自注册、
运行期只读、装配期查名 fail fast,与 model/source 同一惯例):

```go
func init() {
    serving.RegisterProgressHandler("card-steps", func(ctx context.Context,
        conv channel.ConvRef, ev runctx.ProgressEvent) { /* 自行呈现 */ })
    serving.RegisterDecorator("my-card", myCardDecorator) // 见 §3 示例
}
```

```yaml
channels:
  - name: ops-feishu
    type: feishu
    agent: ops-manager
    reply_mode: card
    on_progress: card-steps        # 进度订阅(装了它内置订阅者让位)
    decorator: my-card             # 出站装饰(单一装饰点,见 §3)
    config:
      app_id: "${FEISHU_APP_ID}"
      app_secret: "${FEISHU_APP_SECRET}"
```

两个位置的分工:`on_progress` 管"过程给谁看"(订阅流);`decorator`
管"每条出站消息长什么样"(从文案到整卡结构,全部在这一个点)。

## 3. 出站装饰:单一装饰点

**不区分语义装饰与通道装饰,框架不做任何飞书样式定制**(不加 header、
不做模板、不做表格降级)——呈现完全交给第三方。只有一个装饰点:

```go
// serving.Decorator 应用于每条出站消息(占位卡/终稿/问句/错误),
// 在 channel.Send/Update 之前调用。装饰器读语义事实(Kind/Text/
// Progress/Meta),两种产出:
//   - 改写语义字段(Text 等):适配器按默认方式渲染(飞书 = 最简卡片);
//   - 构造 Native:适配器原样透传(飞书 = 完整卡片 JSON),header/
//     折叠面板/按钮/表格组件想加什么加什么,样式 100% 第三方所有。
type Decorator func(ctx context.Context, conv channel.ConvRef, out channel.Outbound) channel.Outbound

// serving.Binding 增加:
Decorator Decorator // nil = 不装饰,现行为

// 按名注册(配置方接线用;嵌入方直接塞函数值):
func RegisterDecorator(name string, d Decorator)
```

适配器侧唯一的配合:`Outbound.Native` 非 nil 时原样透传,不再 encode:

```go
type Outbound struct {
    Text     string
    Markdown bool
    Kind     string         // processing | answer | question | error(生命周期语义)
    Progress []string       // 过程行事实(框架填充,展示与否装饰器决定)
    Meta     string         // 元信息事实(耗时/调用数)
    Native   map[string]any // 非 nil = 通道原生载荷,适配器原样透传
}
```

装饰器示例(第三方实现 TiDA 式卡片,框架零参与):

```go
serving.RegisterDecorator("my-card", func(ctx context.Context,
    conv channel.ConvRef, out channel.Outbound) channel.Outbound {
    card := map[string]any{
        "config": map[string]any{"wide_screen_mode": true, "update_multi": true},
        "header": map[string]any{ // 处理中灰头、完成蓝头——第三方自己的策略
            "title":    map[string]any{"tag": "plain_text", "content": "运营助手"},
            "template": map[string]string{"processing": "grey", "answer": "blue",
                "question": "orange", "error": "red"}[out.Kind],
        },
        "elements": buildElements(out), // 折叠面板(out.Progress)+ 正文 + note(out.Meta)
    }
    out.Native = card
    return out
})
```

要点:
- **框架给事实,第三方给样式**:Kind/Progress/Meta 是框架保证如实填充
  的语义事实;怎么画(乃至画不画)全在装饰器;
- **一个函数管所有状态**:占位/终稿/问句/错误都过同一个装饰器,按
  out.Kind 分支——生命周期呈现策略集中在一处,不散落;
- **不装 = 现行为**:默认无装饰器,飞书渲染最简 markdown 卡片(现状);
- 框架可另外**导出工具函数**(如 markdown 表格转列表)供装饰器选用——
  是库函数不是管道阶段,用不用第三方定。

## 4. 一次消息的完整旅程(card 模式 + 第三方装饰器)

1. 用户消息进 → dispatcher 构造 `Outbound{Kind: processing}` →
   装饰器(第三方画占位卡,或不管)→ Send → 占位卡;
2. agent 执行,observe 切面发事件给 sink → 内置订阅者节流合并 →
   构造 `Outbound{Kind: processing, Progress: 过程行}` → 装饰器 →
   Update → 卡片过程生长(装了 on_progress 则这步整体由第三方接管);
3. 完成 → `Outbound{Kind: answer, Text, Progress 全量, Meta}` →
   装饰器 → Update 原地收口;挂起 → `Kind: question`;失败 → `Kind: error`。

第三方两个介入深度:装 `decorator`(读事实画卡,生命周期由框架驱动)
→ 再装 `on_progress`(连过程呈现节奏都自己管)。

## 5. 兼容与降级

- 不装 sink / 不装装饰器 / 零值 Outbound = 今天的行为,存量零改动;
- text/stream 模式不经过程订阅(text 无卡可更;stream 有自己的刷新);
- 不支持 Update 的通道:card 退化整段(已实现),过程更新自然失效;
- 装饰器 panic:recover + 用未装饰的原始消息发送 + 记日志——装饰失败
  不能吞消息(诚实优先于好看);
- 飞书 PATCH 频控:内置订阅者节流 ≥2s 且合并;终稿更新永远兜底;
- Native 透传即第三方全责(schema 兼容、客户端版本差异)。

## 6. 落地批次

| 批 | 内容 | 验证 |
|---|---|---|
| 1 | runctx 事件/接收器类型 + observe.ProgressEvents 发射 + 单测 | 假 sink 断言事件序列 |
| 2 | Outbound 语义字段 + Native 透传(feishu)+ Binding.Decorator + 注册表/配置接线 | 单测 + 真机(装饰器画一张带 header 的卡) |
| 3 | dispatcher 内置订阅者(节流把 Progress 填进 processing 更新)+ Binding.OnProgress + placeholder 配置 | 真机长处理看过程生长;第三方订阅示例 |
| 4 | 工具函数库(表格转列表等)+ 使用文档 + interactive 示例装饰器 | 真机完整 TiDA 式卡片 |

## 7. 风险与开放问题

- **队列容量与丢弃率**:默认 64 对 tool/skill 粒度富余(一轮通常
  <30 事件);若未来加模型 token 级事件,粒度换挡时重估容量;
  投递 worker 内 recover,订阅者 panic 记日志不中断投递;
- **过程行信息密度**:只发 tool/skill/model 三档;行数控制是装饰器
  的事(框架给全量事实);
- **卡片组件客户端版本**:Native 里用什么组件第三方自担(table 组件
  旧客户端显示升级提示,collapsible_panel/note 以探针实测为准);
- **装饰器与 Update 的一致性**:同一条消息的占位与收口都过装饰器,
  第三方需保证两次产出的卡片可 PATCH(update_multi 等 config 自己带上,
  文档写明)。
