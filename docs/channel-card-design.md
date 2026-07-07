# IM 通道扩展方案 v2:执行进度流 + 出站装饰链

> 状态:设计稿 v2(2026-07)。v1(模板渲染为中心)已被本版收编:模板
> 渲染降格为"内置装饰器之一"。v2 的立场:**框架给机制(事件流 + 装饰
> 链),呈现策略交给第三方**——TiDA Lens 式富卡片只是这套机制上的一个
> 默认实现。

## 0. 两个诉求与设计立场

1. **底层支持 processing 过程流,第三方可订阅**,如何呈现(更新卡片/
   发进度条/写日志/推别处)由订阅方决定;
2. **飞书发消息前支持对内容做装饰**——原始内容出站前可被第三方改写/
   包装(加标题头/折叠面板/品牌尾注/表格降级)。

立场:两者都是**机制 vs 策略**的切分。框架保证事件如实产生、装饰点
如实存在(Ring 0,代码保证);产生什么样的用户体验是策略,归第三方
(或内置默认策略)。

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
                    │  (卡片过程更新)  (Binding.OnProgress)
                    │
                    └─▶ 终稿 Outbound ─▶ 语义装饰链(通道无关)
                                              │
                                        channel.Send/Update
                                              │
                                  feishu encode() ─▶ 载荷装饰链(卡片 JSON)
                                              │
                                          飞书 OpenAPI
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

## 3. 出站装饰链

### 3.1 两级装饰:语义级(通道无关)与载荷级(通道专属)

**语义装饰**——改的是 `channel.Outbound`(文本/语义字段),不知道飞书:

```go
// serving.Binding 增加:
// Decorators 依次应用于每条出站消息(含占位卡/终稿/问句),返回改写
// 后的消息。典型:追加品牌尾注、注入元信息、敏感词过滤。
Decorators []func(ctx context.Context, out channel.Outbound) channel.Outbound
```

**载荷装饰**——改的是飞书卡片 JSON(encode 之后、POST 之前),第三方
拿到完整卡片结构,可以加 header/折叠面板/note/按钮/任意组件:

```go
// impl/channel/feishu:
// CardDecorator 在卡片 JSON 构建后、发送前依次应用。card 是可变的
// 飞书卡片结构(elements/header/config...);返回 nil 表示放弃装饰。
type CardDecorator func(ctx context.Context, conv channel.ConvRef, card map[string]any) map[string]any

// 注册表模式(init 自注册、运行期只读,与 model/source 同源):
func RegisterCardDecorator(name string, d CardDecorator)
```

```yaml
channels:
  - name: ops-feishu
    type: feishu
    config:
      card_decorators: [table-fallback, brand-header]  # 按名引用,顺序即应用序
```

第三方 Go 侧 `init()` 里 `feishu.RegisterCardDecorator("brand-header", fn)`,
YAML 按名挂载——代码提供能力、配置决定启用,装配期查名 fail fast。

### 3.2 内置装饰器(默认策略,全部可卸载)

| 名 | 级 | 行为 |
|---|---|---|
| `table-fallback` | 载荷 | markdown 表格 → 分组列表(飞书 markdown 组件不渲染表格,真机已证;客户端统一的企业可换 `table-component` 变体) |
| `cards-template` | 载荷 | v1 的模板渲染:按 Outbound.Kind 加 header(标题/主题色)、把 Progress 渲染成折叠面板、Meta 渲染成 note;样式细节读 `config.cards:` 块 |
| (占位文案) | 语义 | `channels[].placeholder` 配置「处理中」文案(dispatcher 级,通道无关) |

默认挂载 `[table-fallback, cards-template]`——开箱即是"标题头 + 执行
过程面板 + 正文 + note"的完整形态;第三方置换 `card_decorators` 列表
即完全接管。

### 3.3 Outbound 语义字段(装饰器的输入面)

沿用 v1 的语义化扩展,作为装饰器能读到的事实:

```go
type Outbound struct {
    Text     string
    Markdown bool
    Kind     string            // processing | answer | question | error(生命周期语义)
    Progress []string          // 过程行(内置订阅者/终稿收口时填充)
    Meta     string            // 元信息(耗时/调用数)
    Native   map[string]any    // 逃生舱:整卡透传,跳过 encode 与全部载荷装饰
}
```

零值 = 现行为;`Native` 与装饰链互斥(透传即第三方全责)。

## 4. 一次消息的完整旅程(card 模式,默认装饰)

1. 用户消息进 → dispatcher 发 `Outbound{Kind: processing}` → 语义装饰
   (占位文案)→ encode → 载荷装饰(cards-template 加灰色 header)→ 占位卡;
2. agent 执行,observe 切面把每次工具/技能调用发给 sink → 内置订阅者
   节流合并 → `Update(processing + Progress)` → 卡片"执行过程"生长;
3. 完成 → `Outbound{Kind: answer, Text, Progress 全量, Meta 耗时}` →
   装饰(蓝 header + 折叠面板收起 + 表格降级 + note)→ 原地更新为终稿;
   挂起 → `Kind: question` 收口;失败 → `Kind: error`。

第三方三个介入深度:换配置(cards:/placeholder)→ 挂自定义装饰器
(读 Kind/Progress 画自己的卡)→ Binding.OnProgress + Native(连生命
周期呈现都自己管)。

## 5. 兼容与降级

- 不装 sink / 不配装饰器 / 零值 Outbound = 今天的行为,存量零改动;
- text/stream 模式不经过程订阅(text 无卡可更;stream 有自己的刷新);
- 不支持 Update 的通道:card 退化整段(已实现),过程更新自然失效;
- 装饰器 panic:recover + 跳过该装饰器 + 记日志,消息必须发出去
  (装饰失败不能吞消息——诚实优先于好看);
- 飞书 PATCH 频控:内置订阅者节流 ≥2s 且合并;终稿更新永远兜底。

## 6. 落地批次

| 批 | 内容 | 验证 |
|---|---|---|
| 1 | runctx 事件/接收器类型 + observe.ProgressEvents 发射 + 单测 | 假 sink 断言事件序列 |
| 2 | Outbound 语义字段 + feishu 载荷装饰链(注册表 + config 接线)+ table-fallback | 单测 + 真机(表格消息) |
| 3 | cards-template 内置装饰器(header/panel/note,cards: 配置) | 真机三态卡片 |
| 4 | dispatcher 内置订阅者(节流更新过程行)+ Binding.OnProgress + placeholder 配置 | 真机长处理看过程生长;第三方订阅示例 |
| 5 | Native 逃生舱 + 使用文档 | 真机全自定义卡 |

## 7. 风险与开放问题

- **队列容量与丢弃率**:默认 64 对 tool/skill 粒度富余(一轮通常
  <30 事件);若未来加模型 token 级事件,粒度换挡时重估容量;
  投递 worker 内 recover,订阅者 panic 记日志不中断投递;
- **过程行信息密度**:只发 tool/skill/model 三档;卡片侧超 N 行折叠
  「…前 K 步省略」;
- **卡片组件客户端版本**:collapsible_panel/note 的最低版本以探针
  实测为准,cards-template 提供 `rich: false` 一键退化纯 markdown;
- **装饰链顺序敏感**:按声明序应用并写入文档;table-fallback 应在
  cards-template 之前(先净化正文再包结构)。
