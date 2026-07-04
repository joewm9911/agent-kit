# smoke —— 端到端冒烟场景:电商运营中台

这棵配置树刻意用满框架全部能力,既是最复杂的示例,也是能力覆盖的活文档。
驱动它的测试在 [config/smoke_e2e_test.go](../../config/smoke_e2e_test.go)。

## 场景结构

```
app.yaml                    2 个 agent + 可靠性 + ${SMOKE_*} 环境占位
agents/
  ops-manager.yaml          运营主管:三域全挂,审批策略/预算/digest/记忆全开
  support-bot.yaml          客服:低预算(预算硬停验证对象)
namespaces/
  catalog.yaml              商品域:react/direct/rewoo/workflow/graph,
                            并行 needs、fork、{$input}、use: 入口、mutating 传播
  marketing.yaml            营销域:reflection/router/plan-execute、
                            调用级 todo、跨 ns skill 引用
  crm.yaml                  客户域:最小形态 + fork,多 agent 共享验证对象
```

## 能力覆盖矩阵(→ 断言所在测试)

| 能力 | 载体 | 测试 |
|---|---|---|
| 三层装配/边界/风险传播/多 agent | 全树 | TestSmokeAssembly |
| graph 并行汇合 + fork 快照 + digest 消化 | price-review | TestSmokeGraphForkDigest |
| 8 种引擎(direct/react/plan-execute/reflection/router/rewoo/workflow/graph) | 各 skill | TestSmokeEngineMatrix |
| RAG(vector 知识库工具,agentic 检索后作答) | faq_bot → tools/kb | TestSmokeEngineMatrix |
| use: 入口引用 | quick-product-qa / audit-product | TestSmokeEngineMatrix |
| 跨 ns skill 引用 | campaign_planner → catalog/price-review | TestSmokeEngineMatrix |
| 调用级 todo + 计划注入 | deep_research | TestSmokeEngineMatrix |
| 轨迹入会话/长期记忆/自动召回/滚动摘要 | ops-manager 多轮 | TestSmokeAgentMemoryLoop |
| 参数级审批(allow 规则/交互拒绝/决策记忆) | apply-price | TestSmokeApprovalPolicy |
| 预算硬停(skill 内部计入) | support-bot | TestSmokeBudgetHardStop |
| 真实模型全链路 | ops-manager + MiniMax | TestLiveMiniMaxSmoke |

未在本场景重复覆盖、由单元测试守护的机制:中断/steering(agent 包)、
失败轮落痕/锚定/轮次锁(agent 包)、重试/超时/转义/override 链(loop/
skill/config 包)。

## 运行

矩阵测试(脚本模型 + 本地 mock 后端,不出网):

```bash
go test ./config/ -run TestSmoke -v
```

真实模型环节(工具后端仍是本地 mock,只有模型是真的):

```bash
MINIMAX_API_KEY=... SMOKE_LIVE=1 go test ./config/ -run TestLiveMiniMaxSmoke -v -count=1
# 海外平台的 key 需另设 SMOKE_MODEL_BASE=https://api.minimax.io/v1
```

## 环境占位

配置里的 `${SMOKE_*}` 占位由测试注入(fail fast:缺一个都无法加载),
含义见 [app.yaml](app.yaml) 头注释。要手工把这棵树当真实应用跑,导出
同名环境变量并把 `SMOKE_API_BASE` 指向真实业务后端即可。
