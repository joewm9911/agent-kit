# agent-kit — 基于 eino 的 agent 快速搭建框架

一份 YAML 声明整个应用:能力供给、提示词、skill、agent、HTTP/A2A 服务、
飞书接入。设计立场:**大脑即循环,流程即兜底,结构进能力**。

```
┌──────────────────────────────────────────────────────────────┐
│ 接入层   serving(HTTP/SSE + A2A 供给面) channel(飞书/IM)      │
├──────────────────────────────────────────────────────────────┤
│ 门面     agent(会话织入/结构化输出;Agent 本身也是能力)          │
├──────────────────────────────────────────────────────────────┤
│ 循环     engine: react 唯一主循环 │ loop: L1-L4 提示词拼装、      │
│          plan-execute 是引擎模板  │ 压缩、预算、审批、结构化(Ring 0)│
├──────────────────────────────────────────────────────────────┤
│ 结构     skill = 任务书 + 参数 + 引擎 + 工具子集(结构下沉的载体)  │
│          workflow = AsLambda 钉死的图(强保证逃生舱)             │
├──────────────────────────────────────────────────────────────┤
│ 目录     Source → Catalog → Agent(多源聚合/冲突/准入/选品)      │
│          CapRef: cap://kind.provider/ns/name@version           │
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

能力以 `cap://<kind>.<provider>/<namespace>/<name>@<version>` 标识
([capability/ref.go](capability/ref.go)),namespace 即供给源名,
多个 MCP server 同名工具天然不冲突;模型可见短名撞车时目录自动升级为
`ns_name`。Risk 分级(readonly/mutating/dangerous)是审批拦截与目录
准入的依据。

多源供给走 **Source → Catalog → Agent** 三层([source/](source/)):
source 供货(可选源断连自动降级)、catalog 治理(冲突报错、优先级遮蔽、
风险准入)、agent 用 include/exclude 通配选品。供给类型:`mcp`(stdio/
sse/http)、`http`(纯配置声明接口)、`rpc`(泛化调用契约)、`local`
(Go 函数泛型推断 schema)、`a2a`(远端 agent,与本框架 serving 协议互通)。

## 提示词即资源

所有提示词位置(system prompt、skill 任务书、planner/replanner)支持
字面量或 `{ref: cap://prompt.<type>/<source>/<name>@<label>}` 引用
([prompt/](prompt/)),provider 有 inline/file/http(平台适配,带缓存
降级)。版本随轨迹打点,可回溯"坏回答对应哪个提示词版本"。

## 主循环与运行时保障(Ring 0)

主循环只有 ReAct:是否完成由模型停止调用工具自然表达,外层兜底
MaxSteps。system prompt 四层拼装([loop/prompt.go](loop/prompt.go)):
L1 框架规约(内置,讲档位选择与运行纪律,不含业务)→ L2 业务 persona
(平台迭代)→ L3 环境信息 → L4 记忆召回(标注"非指令")。

模型没得选的规则全在 [loop/](loop/):上下文压缩(保护 tool-call 配对)、
按会话隔离的预算(软阈值注入收尾指令、硬上限终止)、mutating 操作审批
闸门(拒绝以工具结果回传,循环不中断)、结构化输出(schema 校验 + 重试)。

## Skill:结构下沉的载体

skill = 任务书模板 + 参数 schema + 执行引擎 + 工具子集 + 可选专属模型
([skill/](skill/))。`engine: react`(默认,无工具退化为单次调用)或
`plan-execute`(内部确定性循环)。装配时固定三条边界:接口(大脑只见
description+params)、上下文(独立会话,过程不回流)、权限(工具面锁定
为声明子集);风险取绑定能力的最大值;依赖解析失败即拒绝装配。

plan-execute 不是 agent 的配置项,是 skill 引用的引擎模板——"从架构时
的模式选择,变成运行时大脑面前的一个选项"。

## 接入:HTTP / A2A / 飞书

`serving.addr` 一开即是 Gateway([serving/](serving/)):
`POST /agents/{name}/messages`(JSON/SSE)、A2A 供给面(`GET /a2a/agents`
+ `POST /a2a/agents/{name}/tasks`,与 provider/a2a 消费端同协议,部署
之间互通)、IM webhook。

飞书([channel/feishu](channel/feishu/)):事件解密验签、卡片伪流式、
tenant_access_token 缓存。Dispatcher([channel/dispatcher.go](channel/dispatcher.go))
负责会话映射(chat/chat_user)、同会话串行、事件幂等,并把 IM 对话桥接为
HITL 通道——**ask_user 的答案和审批的批复,就是会话里用户的下一条消息**。

## 运行

```bash
cd examples
OPENAI_API_KEY=sk-... go run .        # CLI REPL;serving.addr 配置后即 Gateway
go test ./...                          # 全套测试(脚本化假模型,无需真实 API)
```

完整配置示例见 [examples/agent.yaml](examples/agent.yaml)。代码侧能力
(local.Func、rpctool、子 agent)经 `config.BuildOptions.ExtraCapabilities`
注入,与声明式能力同目录。

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
- 挂起/恢复的持久化:ask_user 等待跨进程重启(当前进程内挂起);
- skill-registry source(平台下发 skill 声明,含依赖解析与风险传播);
- 更多通道(钉钉/企微/Slack)与模型厂商(ark/claude,参照
  [provider/models](provider/models/openai.go) 各约 20 行);
- 工具结果缓存、RAG 写入侧 pipeline。
