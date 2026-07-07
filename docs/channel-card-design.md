# IM 富卡片方案:语义化回复 + 通道模板渲染 + 第三方定制

> 状态:设计稿(2026-07)。目标形态参照:标题头 + 可折叠"执行过程"面板 +
> 正文区 + 底部元信息(TiDA Lens 式);要求方案通用——语义与样式分离,
> 飞书只是第一个渲染后端,钉钉/Slack 适配器接同一套语义。

## 1. 背景与问题

card 模式(占位卡片→原地更新)已落地,但卡片本身是写死的最小形态:

| 现状 | 问题 |
|---|---|
| 占位文案「⏳ 处理中...」硬编码在 dispatcher | 业务无法换文案/样式 |
| 卡片 = 单 markdown 元素,无标题头/无结构 | 与目标形态差距大;处理中和终稿视觉无区分 |
| 处理过程对用户完全黑盒 | 长处理(技能子循环 30s+)只有一句"处理中" |
| markdown 组件不渲染表格(飞书限制,真机已证) | 模型输出的表格原样吐管道符 |
| `Outbound{Text, Markdown}` 二元契约 | 任何富样式都没有表达位 |

## 2. 真机探测结论(测试群实测)

| 组件 | API | 渲染 | 结论 |
|---|---|---|---|
| header(标题 + 主题色模板) | ✅ code 0 | 待确认(探针A) | 处理中/完成态可用不同色区分 |
| collapsible_panel(折叠面板) | ✅ code 0 | 待确认(探针B) | "执行过程"的载体 |
| note(底部灰字元信息) | ✅ code 0 | 待确认(探针C) | 耗时/调用次数/会话号 |
| markdown 表格语法 | — | ❌ 管道符原样显示 | 必须降级处理 |
| table 组件 | ✅ code 0 | ⚠️ 旧客户端显示"请升级客户端" | 不能作为默认路径 |
| PATCH 更新(update_multi) | ✅ 已在用 | ✅ | 全部状态流转的基础 |

## 3. 设计:三层分工(对照分层规范)

原则:**协议层表达语义,适配层拥有样式,编排层驱动状态**。任何一层不
越界——dispatcher 不知道"蓝色标题头",feishu 适配器不知道"轮次挂起"。

### 3.1 protocol/channel:语义化 Outbound(通道无关)

```go
// Outbound 是要发出的一条消息(语义面,不含任何通道样式)。
type Outbound struct {
    Text     string
    Markdown bool

    // Kind 是消息的生命周期语义,通道适配器据此选择呈现模板:
    //   ""(默认,普通消息)| processing(处理中占位)|
    //   answer(终稿)| question(向用户提问/审批)| error(失败)
    Kind string

    // Progress 是执行过程记录(每行一步,如「✓ quick-product-qa (7.8s)」)。
    // 支持富呈现的通道渲染为可折叠面板;不支持的通道忽略或附在正文后。
    Progress []string

    // Meta 是底部元信息(耗时/调用次数等),通道渲染为 note/脚注,可忽略。
    Meta string

    // Native 是通道原生载荷逃生舱:非 nil 时适配器直接透传(飞书 = 完整
    // 卡片 JSON),以上语义字段全部失效。第三方要完全接管卡片时用。
    Native map[string]any
}
```

要点:
- 新字段全部**可选**,零值行为与现状完全一致(既有通道/测试零改动);
- `Kind` 是语义枚举不是样式名——"processing 长什么样"由适配器配置决定;
- `Native` 是逃生舱而非主路径:用它意味着放弃跨通道可移植性,自担
  schema 兼容(文档标注)。

### 3.2 impl/channel/feishu:模板渲染器 + 卡片配置

`encode()` 升级为按 Kind 查模板渲染:

```yaml
channels:
  - name: ops-feishu
    type: feishu
    config:
      app_id: ...
      # 卡片模板:按 Kind 配置,缺省有内置默认(向后兼容现状)
      cards:
        processing:
          header: {title: "运营助手", template: "grey"}   # 灰头 = 进行中
          body: "⏳ {{text}}"                              # 文案可替换
        answer:
          header: {title: "运营助手", template: "blue"}    # 蓝头 = 完成
          progress_panel: true      # Progress 渲染为折叠面板(默认收起)
          progress_title: "执行过程"
          note: true                # Meta 渲染为底部 note
        question:
          header: {title: "需要你确认", template: "orange"}
        error:
          header: {title: "处理失败", template: "red"}
```

渲染结构(answer 完整形态,即目标截图的结构):

```
┌ header(title + template 色)          ← cards.<kind>.header
├ collapsible_panel「执行过程」(收起)   ← Outbound.Progress
├ hr
├ markdown 正文                          ← Outbound.Text(表格已降级)
└ note 灰字                              ← Outbound.Meta
```

**markdown 表格降级**(适配器内,对上游透明):检测 `|---|` 表格块,
按配置二选一:
- `table_render: fallback`(默认):转"分组列表"(表头加粗做小节,每行
  转「- 字段: 值」),所有客户端可渲染;
- `table_render: component`:转卡片 table 组件(新客户端原生表格,旧
  客户端见升级提示)——企业内客户端版本统一时启用。

### 3.3 serving/dispatcher:生命周期状态机(通道无关)

card 模式的状态流转,每步只填语义字段:

```
收到消息 ──Send(Kind=processing)──▶ 占位卡
    │
    ├─ 过程事件(节流 ≥2s)──Update(processing + Progress 增量)──▶ 卡片过程行生长
    │
    ├─ 完成 ──Update(Kind=answer, Text=终稿, Progress=全过程, Meta=耗时)──▶ 终稿卡
    ├─ 挂起 ──Update(Kind=question, Text=等待提示)──▶ 等待卡(问句仍是独立消息)
    └─ 失败 ──Update(Kind=error, Text=错误说明)──▶ 失败卡
```

**过程事件来源**(Ring 0,不靠模型自觉):`runctx` 增加进度接收器接口,
engine 的工具调用中间件(已包裹每次工具执行)上报"开始调用 X / X 完成
(耗时)";dispatcher 在 card 模式下往 ctx 装一个带节流的 sink,收到事件
就更新卡片。分层依据:core 定义 sink 类型 → runtime/engine 上报 →
serving 消费,方向合法;不装 sink 时零开销(现状路径不变)。

### 3.4 占位文案的归属

「处理中」的**文案**是绑定级配置(通道无关):`channels[].placeholder`;
「处理中卡片」的**样式**是通道级配置(`config.cards.processing`)。
两级各自缺省,互不依赖。

## 4. 第三方定制的三个层级

| 层级 | 手段 | 适用 | 成本 |
|---|---|---|---|
| L1 配置模板 | YAML `cards:` 块(标题/主题色/文案/开关) | 绝大多数品牌化需求 | 零代码 |
| L2 Native 透传 | 构造 `Outbound.Native` 完整卡片 JSON | 完全接管单条消息(营销卡片/按钮交互) | 自担飞书 schema |
| L3 自定义渲染器 | 适配器暴露 `RegisterCardRenderer(kind, func)` | 整类消息的程序化定制(动态按钮/图表) | Go 代码 |

L3 按 YAGNI 缓建:先落 L1+L2,出现真实诉求再开 L3(登记)。

## 5. 兼容与降级

- `Outbound` 新字段全零值 = 现行为,`text`/`stream` 模式不动;
- 不支持 Update 的通道:card 模式已有的整段退化路径不变;
- Progress/Meta 对不支持富呈现的通道是可忽略语义(适配器自行丢弃);
- 卡片模板配置缺省 = 内置默认(与今天的卡片一致),增量启用;
- 挂起模式(suspendKV)与过程更新兼容:挂起发生时占位卡收口为
  question 态(已实现的行为,换个模板而已)。

## 6. 落地批次

| 批 | 内容 | 验证 |
|---|---|---|
| 1 | Outbound 语义字段 + feishu 模板渲染(header/panel/note)+ cards 配置 | 单测(渲染快照)+ 真机三态卡片 |
| 2 | markdown 表格降级(fallback 列表形态) | 单测 + 真机(商品清单场景) |
| 3 | 过程事件 sink(runctx + engine 中间件)+ dispatcher 节流更新 | 单测 + 真机(技能长处理看过程行生长) |
| 4 | Native 逃生舱 + 文档 | 真机一张全自定义卡 |

批 1/2 独立可用(终稿卡就有完整形态,过程面板先随终稿一次性给出);
批 3 才有"处理中过程实时生长"。

## 7. 风险与开放问题

- **卡片组件的客户端版本差异**:table 组件旧客户端不可用(实测);
  collapsible_panel/note 的最低版本待探针确认——若用户群客户端老旧,
  模板要能整体降回纯 markdown(配置 `rich: false` 一键退化)。
- **过程行的信息密度**:每次工具调用都上报会很长;v1 只报
  工具/技能级(不报模型轮次),超过 N 行折叠为「…前 K 步已省略」。
- **更新频控**:飞书 PATCH 有 QPS 限制,过程更新节流 ≥2s 且合并批量,
  终稿更新始终最后一次兜底(过程刷新失败不影响终稿)。
- **多通道语义漂移**:钉钉/Slack 的"卡片"能力不对齐,Progress/Meta
  必须保持"可忽略"语义,禁止任何通道把它们变成必需。
