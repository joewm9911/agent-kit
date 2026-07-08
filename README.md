# agent-kit — 基于 eino 的 agent 快速搭建框架

声明式定义整个应用:能力供给、提示词、skill、agent、HTTP/A2A 服务、
飞书接入。配置按所有权切分为三层文件(app / agents / namespaces),
也支持单文件形态。设计立场:**大脑即循环,流程即兜底,结构进能力**。

```
┌──────────────────────────────────────────────────────────────┐
│ 接入层   serving(HTTP/SSE + A2A 供给面) channel(飞书/IM)      │
├──────────────────────────────────────────────────────────────┤
│ 门面     agent(会话织入/结构化输出;Agent 本身也是能力)          │
├──────────────────────────────────────────────────────────────┤
│ 循环     engine: react 唯一主循环 │ loop: L1-L4 提示词拼装、压缩、 │
│          plan-execute 是引擎模板  │ 预算、审批、重试、超时(Ring 0) │
├──────────────────────────────────────────────────────────────┤
│ 结构     namespace: tools(ns 内共享)→ components(执行单元声明)  │
│          → skills(对外产品:参数 + DAG 编排,唯一进目录的单元)    │
├──────────────────────────────────────────────────────────────┤
│ 目录     Source → Catalog → Agent(多源聚合/冲突/准入/选品)      │
│          CapRef: cap://kind/domain/name@version                │
├──────────────────────────────────────────────────────────────┤
│ 供给     mcp │ http │ rpc │ local │ a2a │ prompt │ secrets      │
├──────────────────────────────────────────────────────────────┤
│ 底座     eino(compose.Graph / react / 组件 / callbacks / 流式)  │
└──────────────────────────────────────────────────────────────┘
```

## 核心抽象:一切节点皆能力,一个能力两种形态

[capability.Capability](capability/capability.go) 是唯一的中心接口:

```go
type Capability interface {
    Meta() Meta                             // Ref、描述、参数 schema、Risk
    AsTool(ctx) (tool.BaseTool, error)      // 工具形态:大脑决定何时调用
    AsLambda(ctx) (*compose.Lambda, error)  // 节点形态:流程决定何时执行
}
```

工具、模型、记忆、RAG、skill、workflow、完整 Agent 全部实现它。同一个
能力既能进 ReAct 循环(动态编排),也能被钉进图里(静态编排)——这个
选择是**部署时的配置,不是架构时的承诺**。

## CapRef 协议与三层供给

能力以 4 段 `cap://<kind>/<domain>/<name>@<version>` 标识
([capability/ref.go](capability/ref.go))。kind 是对象类别,domain 是该
kind 下 name 的归属域(callable→源名/ns;store/retriever→存储用途;
prompt→prompt 源)。**去掉了旧的 provider 段**——供给方类型(mcp/http…)
只在配置期选工厂,运行时只有统一的 Capability,写进 ref 是泄漏执行期
不存在的区分。两条不变式:**kind 段永远精确**(是 kind 优先解析的前提,
`*`-kind 无法选解析域;domain/name 可通配),**Key 不含 version**(版本
共存靠 registry)。模型可见短名撞车时目录自动升级为 `domain_name`。
Risk 分级(readonly/mutating/dangerous)是审批拦截与目录准入的依据。

kind 二分:**可调用能力**(进 include:tool/skill/component/agent)与
**可命名对象**(装配注入槽:prompt/store/retriever)。model 走内联
ModelConfig,不入 cap 体系(从不被引用)。

多源供给走 **Source → Catalog → Agent** 三层([source/](source/)):
source 供货(可选源断连自动降级)、catalog 治理(冲突报错、优先级遮蔽、
风险准入)、agent 用 include/exclude 通配选品。供给类型:`mcp`(stdio/
sse/http)、`http`(纯配置声明接口)、`rpc`(泛化调用契约)、`local`
(Go 函数泛型推断 schema)、`a2a`(远端 agent,与本框架 serving 协议互通)、
`vector`(向量知识库检索,即 RAG——是 tools 层的一种工具而非独立组件;
内置词法保底后端,真实向量库经 vector.RegisterBackend 接入;离线摄入
不归框架管)。

## 提示词即资源

所有提示词位置(system prompt、skill 任务书、planner/replanner)支持
字面量或 `{ref: cap://prompt/<source>/<name>@<label>}` 引用
([prompt/](prompt/)),provider 有 inline/file/http(平台适配,带缓存
降级)。版本随轨迹打点,可回溯"坏回答对应哪个提示词版本"。

## 主循环与运行时保障(Ring 0)

主循环只有 ReAct:是否完成由模型停止调用工具自然表达,外层兜底
MaxSteps。system prompt 四层拼装([loop/prompt.go](loop/prompt.go)):
L1 框架规约(内置,讲档位选择与运行纪律,不含业务)→ L2 业务 persona
(平台迭代)→ L3 环境信息 → L4 记忆召回(标注"非指令")。

模型没得选的规则全在 [loop/](loop/):上下文压缩(保护 tool-call 配对,
低频一次性事件、前缀稳定不打爆 prompt cache)、按会话隔离的预算(软阈值
注入收尾指令、硬上限终止,skill 内部调用同样计入)、审批闸门(参数级
策略规则 + 会话级决策记忆,拒绝以工具结果回传、循环不中断)、瞬时错误
重试与工具超时、结构化输出(schema 校验 + 重试)。

**审批策略**(`approval` 模块:`mode` + `rules` + `remember`):静态 Risk
回答不了"同一工具因参数而异的危险性",规则把放行下沉到 (能力, 参数) 粒度——
`{ref: "cap://tool/*/send_message", args: {to: "team-*"}, action: allow}`;
首条命中生效,无命中回落 ask;`remember: true` 启用"本会话总是允许/拒绝"。

**中断与驾驶**:运行中的循环可被叫停与插话([loop/control.go](loop/control.go))。
IM 里"停止"类消息旁路会话串行队列即时中断;「插话:」前缀的内容随下一个
工具结果送达模型。HTTP 侧 `POST /agents/{name}/control`。

**挂起/恢复**([suspend/](suspend/)):配置 `suspend.dir` 后,ask_user 与
审批等待不再阻塞 goroutine——交互点持久化挂起、整轮退栈,答案到达
(跨小时/跨天/跨进程重启)后原输入重放:交互与 mutating 效果按确定性键
从日志命中,已批准的操作不会二次执行、不会重复提问;重放分叉时退化为
重新提问,失败模式安全。

**会话记忆含轨迹**:每轮的工具调用与结果(`record_tools: summary` 默认)
随会话持久化,下一轮模型知道自己做过什么、看到过什么,"继续"可接续。

**长期记忆分作用域**([memory/](memory/)):`memory_save/search` 按
`scope` 隔离归属——用户私有(by 终端用户)、共享域知识、会话临时。
读写不对称是有意的:用户面 agent 的对话写入只落用户桶(写收窄),
召回同时覆盖用户桶与共享池(读放开)——共享知识对所有用户可见,但
对话里的模型碰不到共享池的写入权。往共享池写靠三条**显式**路径:
`seed` 装配期灌入、宿主代码 Put、或某个特权 agent 显式声明
`write_scope: shared`——不是模型运行时自选。终端用户身份由 serving 的
`user` 字段 / 飞书发送者 open_id 提供,缺身份时用户记忆写入 fail fast,
不静默落进共享池。会话召回策略([session/](session/) 的 Retriever)
可注册替换,词法 bigram 是缺省保底。

**分布式存储(可换后端)**:四类内部状态——session(会话历史)、memory
(长期记忆)、todo(计划清单)、result(digest 大结果暂存)——都能外置到
共享后端。多副本 serving / 进程重启下,包级内存会让「A 副本写、B 副本读
不到」破坏一致性;外置后键按 agent/会话隔离、跨副本一致。KV 家族
(todo/result,见 [store/](store/))的写是**原子读改写**原语(inmemory
用 mutex,redis 用 WATCH/MULTI 乐观锁 + 抖动退避),多副本并发不丢更新。
用法:agent 层 `stores:` 只声明具名实例 `{name, kind, type, config, ttl}`;
**四大模块各自独立配置**(`session` 会话短期记忆 / `memory` 长期记忆 /
`todo` 计划 / `digest` 大结果暂存),各自的 `store:` 槽用
`cap://store/<kind>/<name>` 引用实例。换后端=改 type、或指向另一实例;
跨 agent 共享=各自指同一实例。裸 `store: file` 作缺省简写,存量零迁移。
redis 后端空导入 [provider/redisstore](provider/redisstore) 即为四类
（session / memory / todo / result）一并开启分布式存储；memory 的 redis
后端是关键词匹配(向量检索另属一族,由 qdrant 等提供)。

```yaml
stores:                       # 仅定义
  - {name: sess,  kind: session, type: redis, config: {addr: 127.0.0.1:6379}}
  - {name: ltm,   kind: memory,  type: redis, config: {addr: 127.0.0.1:6379}}  # 或 qdrant 走向量
  - {name: plans, kind: todo,    type: redis, config: {addr: 127.0.0.1:6379}}
  - {name: cache, kind: result,  type: redis, config: {addr: 127.0.0.1:6379}}

loop:                         # 执行画像:压缩归 loop(主循环与 component 共用)
  max_steps: 30
  compaction: {max_messages: 30, keep_recent: 10}
session:                      # 会话短期记忆
  store: cap://store/session/sess
  window: 40
  recall: {top_k: 3}
memory:                       # 长期记忆(独立模块)
  store: cap://store/memory/ltm
  tools: true
  recall: {top_k: 3}
todo:                         # 计划清单
  store: cap://store/todo/plans
digest:                       # 大结果消化/暂存
  over: 4000
  store: cap://store/result/cache
```

## 命名空间:声明与使用分离的三层结构

配置的主路径是 `namespaces:`([config/namespace.go](config/namespace.go)),
每个命名空间三层,逐层回答一个问题:

- **tools** — 有什么原子能力(mcp/http/rpc 声明),ns 内共享,对外不可见;
- **components** — 执行单元是什么("能力声明"),ns 内复用,不进全局目录。
  两族形态,**engine 必填**(执行形态决定成本模型与行为保证,不做隐式
  默认)。循环族(prompt + tools):`direct`(单发:一次调用+一轮工具+
  收尾)| `react`(自主循环)| `plan-execute`(规划循环)| `reflection`
  (产稿→评审→修正,轮次代码钉死)| `router`(一次分诊调用路由到
  工具面上的能力)| `rewoo`(一次规划生成带 {eN} 引用的工具计划,
  按依赖并行执行,一次求解,全程两次模型调用);编排族(steps,无脑
  钉死):`workflow`(纯顺序,禁 needs)| `graph`(DAG,可并行);
- **skills** — 对外长什么样、怎么串(描述 + 参数,即"能力使用"),唯一
  进全局目录、被 agent 发现的单元。执行可内联 `steps`,也可 `use:
  components/<名>` 整体委托给一个编排族 component——skill 与私有图的
  区别只剩可见性。

边界规则在装配期落实:工具与 component 不出命名空间;跨命名空间只能
引用 `cap://skill`——skill 是命名空间的唯一公开接口。

**skill 的执行语义是 DAG**([skill/graph.go](skill/graph.go)):steps
声明为带 `needs` 的列表,缺省依赖上一步(退化为串行链),显式 `needs`
表达并行与汇合;步骤支持 `timeout`/`retry`。装配期校验依赖存在、无环、
模板引用 ⊆ needs 传递闭包(数据流与控制流一致,并行下无竞态读)。
state = 参数 ∪ {步骤名: 输出},每次调用一份、单写者无冲突。运行期没有
大脑做路由,执行路径是强保证的。

## Skill 三边界与治理下沉

skill/component 装配时固定三条边界:接口(大脑只见 description+params)、
上下文(独立会话,过程不回流)、权限(工具面锁定为声明子集);风险取
绑定能力的最大值;依赖解析失败即拒绝装配。

Ring 0 的运行时保障**覆盖每一层大脑,不止主循环**:审批闸门与预算门闸
由 agent 在每次运行装入 ctx(`loop.WithApprovalMode`/`loop.WithBudget`),
component 内部的 mutating 工具调用同样过闸、内部模型调用同样计入调用方
会话预算——同一 skill 被不同策略的 agent 复用时各自独立生效。

plan-execute 不是 agent 的配置项,是 component 引用的引擎模板——"从架构
时的模式选择,变成运行时大脑面前的一个选项"。平铺的 `skills:`/`workflows:`
段保留为兼容路径。

## 可靠性(Ring 0)

`reliability:` 段(执行画像的一部分,`{retry, tool_timeout}`,零值即默认,
可逐层降级)声明可靠性策略([loop/retry.go](loop/retry.go)、
[loop/timeout.go](loop/timeout.go)):模型调用对限流/瞬时服务端/网络错误
指数退避重试(默认 3 次尝试,确定性错误不重试);工具单次调用超时
(默认 5 分钟,超时以结果回传模型换路径推进,循环不中断;审批等待
不计入超时)。编排步骤的 `timeout` 超时则视为步骤失败,确定性中断整图。

## 上下文卫生:digest 与 fork

两个正交开关防止上下文污染与背景丢失([loop/digest.go](loop/digest.go)、
[loop/fork.go](loop/fork.go)):

- **digest(结果消化)**:agent 级 `digest.over` 阈值之上的工具结果先落
  暂存(后端 `digest.store` 可外置),由模型带着当前任务提取要点后入
  上下文,附取回指针——搜索、捞日志等大数据量工具不再挤爆窗口;摘要
  不够时模型可用内置 `read_result(id, offset)` 分页翻原文。消化是有损
  优化,失败退回原样,截断闸兜底;`result:raw` 标签可豁免。`digest`(over/
  truncate/store)是执行画像的一部分,主循环与 component 逐层就近降级。
- **fork(上下文继承)**:带内部循环的能力默认从零起步(fresh,背景靠
  args 转述);步骤声明 `context: fork` 后,内部循环以"调用方对话快照 +
  任务书"起步——背景无损继承,隔离方向不变(过程不回流,只返回结果)。
  fork 复制一份调用方历史 token 且吃不到其 prompt cache,默认 fresh。

编排步骤之间的数据流走 state 变量、不进模型上下文,大数据管道天然
免疫——重数据流程优先下沉到 steps。

todo(计划外化)只属于主循环:agent 默认挂载(harness 强制纪律:
写入校验、每轮注入、卡住提醒),子 agent 按执行域隔离;component
默认没有(能结构化的计划用 steps/引擎表达)。react component 可
`todo: true` 挂**调用级临时清单**——键为本次执行域、调用结束即弃,
宿主计划不受影响,组件保持无状态可重入;这是给确实拆不动的研究型
长循环的例外通道,不是常规选项。

信息传递的通道谱系(成本与信息量递增,均为使用点显式声明):

```
params(主脑转写,有损) → {$input}(保留变量:用户本轮原话,框架直取)
→ fork(全量对话快照) │ 大内容旁路:digest 指针 + read_result 点对点拉取
```

`{$input}` 在步骤 args 模板里引用,装配期校验($ 前缀仅限保留变量,
params/步骤名禁用 $ 开头),调用方传入同名键不能顶掉框架注入值。

## 接入:HTTP / A2A / 飞书

`serving.addr` 一开即是 Gateway([serving/](serving/)):
`POST /agents/{name}/messages`(JSON/SSE)、A2A 供给面(`GET /a2a/agents`
+ `POST /a2a/agents/{name}/tasks`,与 provider/a2a 消费端同协议,部署
之间互通)、IM webhook。配置 `suspend:` 后 `/messages` 支持持久化挂起:
ask_user/审批不占请求等待,响应 `{status: "waiting", question}`,同会话
的下一个请求即答案(跨进程重启/多副本可恢复,与 IM 通道共用同一后端)。

飞书([channel/feishu](channel/feishu/)):事件解密验签、卡片伪流式、
tenant_access_token 缓存。Dispatcher([channel/dispatcher.go](channel/dispatcher.go))
负责会话映射(chat/chat_user)、同会话串行、事件幂等,并把 IM 对话桥接为
HITL 通道——**ask_user 的答案和审批的批复,就是会话里用户的下一条消息**。

## 配置的三层文件形态

配置按所有权切分([config/app.go](config/app.go)),每层文件回答一个问题:

```
app.yaml                 装配成什么进程(部署拥有):secrets/serving/channels/
                         observability + 执行画像基线(model/loop/reliability/
                         digest/step_defaults)+ agents 接线,业务含量为零
agents/<name>.yaml       对外是什么产品(产品面拥有):执行画像 + 记忆/预算/审批
                         + namespaces 关联(挂载即获得其全部导出 skill)
namespaces/<name>.yaml   有什么能力(域团队拥有):tools/components/skills
                         (+ 可声明执行画像,但不含 model——能力不可自指)
```

约定:文件名即名字;相对路径相对引用它的文件解析。namespace 是库,
agent 挂载时按解析出的**执行画像**实例化(源连接按文件缓存共享);跨 ns 的
cap://skill 引用在同一 agent 的挂载集合内按关联顺序解析。

**统一执行画像 + 五级降级**([config/profile.go](config/profile.go)):一套
`Profile`(model/loop/reliability/digest/step_defaults)四层共用,声明在哪层
就在哪层生效,缺失则逐**字段**就近降级(nil=继承)。component 生效值从高到低:

```
agent给该ns指定(per-mount) → component → namespace → agent自己 → app
```

主循环取 `app.merge(agent自己)`。**model 特例**:能力不能自指模型(部署/成本
决策由集成方定),ns/component 不参与,链退化为 `per-mount → agent自己 → app`;
典型场景 agent 挂 catalog(便宜模型)+ research(强模型),靠给各 namespace
per-mount 指定 model 实现。**会话状态(session/memory/todo)与治理边界
(approval/budget/structured_output)不进画像链**:app→agent 整块降级、不下沉
component——那是 agent/部署持有的安全边界,库不能给自己放权。

per-mount 覆盖写法(mount 条目从裸路径升为路径 + 覆盖画像):

```yaml
namespaces:
  - ../namespaces/catalog.yaml            # 仅路径
  - path: ../namespaces/research.yaml     # 给这个域单独指定强模型 + 步数
    model: {provider: openai, config: {...}}
    loop:  {max_steps: 5}
```

单文件形态(`config.Load`+`Build`)保留为兼容路径。

## 运行

```bash
cd examples
MINIMAX_API_KEY=... go run .          # CLI REPL;serving.addr 配置后即 Gateway
go test ./...                          # 全套测试(脚本化假模型,无需真实 API)
```

完整配置示例见 [examples/app.yaml](examples/app.yaml)、
[examples/agents/](examples/agents/)、[examples/namespaces/](examples/namespaces/)。
代码侧能力(local.Func、rpctool、子 agent)经
`config.BuildOptions.ExtraCapabilities` 注入,与声明式能力同目录。

## 三环边界(什么必须写死)

判据:**"模型不遵守就会出事"的东西和"解释协议本身"的东西写死;
工具面上的一切只是给大脑的选项,选项不能承载保证。**

- **Ring 0(内核)**:主循环、Capability 契约、历史织入/MaxSteps/预算/
  审批闸门/压缩、目录治理规则、观测切面;
- **Ring 1(代码扩展点,registry 注册)**:引擎模板、source/prompt/
  channel/model 的类型适配器、存储后端;
- **Ring 2(纯配置)**:能力实例、skill、提示词、agent、策略值、版本通道。

日常迭代应 95% 落在 Ring 2;若业务需求经常要动内核,说明协议漏了东西,
应回来改协议而不是打补丁。

## Roadmap

- 评测框架:基于轨迹 JSONL 的回放与断言(轨迹格式已落地);
- 超长工具结果的取回路径(截断部分落盘,模型按需分段读取);
- skill-registry source(平台下发 skill 声明,含依赖解析与风险传播);
- 更多通道(钉钉/企微/Slack)与模型厂商(ark/claude,参照
  [provider/models](provider/models/openai.go) 各约 20 行);
- 工具结果缓存、RAG 写入侧 pipeline;编排步骤的条件分支(when)。
