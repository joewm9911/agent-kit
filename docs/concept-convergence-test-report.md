# 概念收敛测试报告

> 日期:2026-07-17;方案:concept-convergence-plan.md;模型:MiniMax-M2.7(真机)。
> 结论先行:**离线全量 0 FAIL,真机冒烟 17/18 PASS(1 例既定条件跳过),
> 四组 A/B 复跑全部不低于收敛前基线,interactive 场景关键链路全部走通。**
> 收敛形态(skill=过程卡 / sub-agent=同构隔离循环 / 编排下沉 eino)成立。

## 1. 交付范围

| 批 | 内容 | 提交 |
|---|---|---|
| 批1-3 | 引擎塌缩(仅剩 react)+ Declaration→过程卡 + AgentDecl/BuildAgent(Kind:"agent")+ namespace 减负 + smoke 迁移 | 37c3cd0 |
| 批4a | interactive 六域样板迁移(过程卡 + sub-agent,regions 移除) | 9e61f7f |
| 批4b | examples/pipeline 硬编排样例(compose+AsLambda)+ L1 词汇收敛 | 74d8f82 |
| 批5 | live 夹具适配 + 本报告 | c135337 起 |

净删约 2000 行(graph 627 + 范式引擎 ~700 + steps 配置面 ~400 + 其余);
配置词汇删除:engine/engine_config/steps/needs/args/output/step_defaults/
use/export/imports/components。误写全部装配期报错、文案自带迁移路径。
store 槽 / include-exclude / approval 模式语法 / cap://prompt 保持现状
(已配置在用的面不折腾)。

## 2. 离线回归

- 全量 `go test ./...` **0 FAIL**;`-race`(skill/loop/config)干净;
  layering 守卫通过;
- 硬切错误矩阵单测锁定:components/imports/steps/use/engine/output/
  deliver(skill 上)/step_defaults/mode/max_steps 十类误写各自报错指路;
- 新形态单测:过程卡装配与渲染、sub-agent fork/fresh 消息形状、
  Kind:"agent" 身份、卡工具直挂幂等、subagent 工具不入目录(权限边界)。

## 3. 真机冒烟(TestLiveSmoke,253s)

17/18 PASS。react 循环、会话记忆、过程卡+digest、todo 纪律、长期记忆、
审批(interactive + allow 规则 + deny 规则)、预算硬停、结构化输出、
上下文压缩、窗外召回、gateway+A2A、副本重启、exectool、skillpack、
中断、轨迹落盘全部通过;13_SuspendResume 条件跳过(模型未走 ask_user
挂起路径直接作答,采样行为,既定跳过条件)。

## 4. A/B 复跑(收敛后 vs 收敛前基线)

| 尺子 | 收敛前基线 | 收敛后 | 判读 |
|---|---|---|---|
| inline 过程卡完成率(n=6) | 6/6,调用 3.0 | **6/6,调用 3.0**(subloop 臂 5/6、4.0) | 持平,-25% 成本保持 |
| deliver 保真 行保留(n=6) | 通道 30/30 vs 基线 25.8/30 | **通道 30.0/30 列 7/7** vs 基线 16.3/30 | 通道满分不回退 |
| deliver 引用形态(n=6) | 引用 5/6 | **引用 5/6 复述 1/6 附件逐字节一致 5/6** | 持平 |
| 交互记忆零重问(n=3) | 2/3(采样波动) | **3/3** | 不回退 |
| delegate 委派观察(n=3) | 3/3 完成 0/3 委派 | **3/3 完成 0/3 委派(直扫 1/1/1)** | "正确地不委派"稳定 |

## 5. interactive 真机场景(CLI 管道驱动,redis 存储)

装配面:6 sub-agent(`cap://agent/*`)+ 15 过程卡/skillpack + 直挂工具,
一次装配零报错。

| 场景 | 链路 | 结果 |
|---|---|---|
| 比价核查 | 主循环直接 search+get_product → 毛利率 38% | ✅ 数字准确 |
| 音频品类销售报表 | **sub-agent(sales-report)+ deliver 直达**:内部 sales_summary + 3×get_sales,`[交付物#d1]` 存底,终答引用 #d1 只做导读,报表原文随行零转写 | ✅ 旗舰链路 |
| 客户 C3 订单诊断 | **sub-agent(order-inquiry)隔离执行**:内部 list_orders,诊断口径自declare,宿主收敛结论 | ✅ 隔离+边界纪律 |
| CANARY-1 调价 | 模型先核实 → mock 后端无此 SKU → 如实报"不存在",未盲调 | ✅ 先核实后执行 |
| P103 调价 89 | 模型算出毛利 -13.5% 先要确认 → 确认后 **apply-price 卡 → 指引 → update_price → 审批闸拦截**(无批准输入,EOF)→ 如实报失败 | ✅ 卡→工具→审批全链 + 诚实汇报 |

第五个场景把新形态的完整机制在一条链上展示:过程卡返回指引、主循环
照指引调 mutating 工具、审批闸(规则挂在 `cap://tool/shop/update_price`)
拦截、失败如实回报——审批放行路径由 TestLiveSmoke 06(interactive
approve + allow 规则)覆盖。

## 6. pipeline 样例(真机)

examples/pipeline 单次真机运行通过:顺序链(authz 纯代码门禁 →
分析 sub-agent → audit 落账)与并行汇合(两 sub-agent 并发 +
compose.Parallel 纯代码合并)均产出正确结论——"节点内有脑、节点间
无脑"的硬编排姿势可跑通,作为 steps 移除后的指路样例。

## 7. 遗留与观察

- 比价场景模型直接用了直挂工具而未先调 price-check 卡(工具在面上,
  行为合法且结果正确);卡的价值在无关工具多、需要流程纪律的任务上,
  与 inline A/B 的贯彻率结论一致,不构成回退;
- CANARY 灰度 allow 规则在 CLI mock 后端无 CANARY SKU,未走到免批
  路径(模型先核实挡住了);该规则由 TestLiveSmoke 06 真机覆盖;
- 13_SuspendResume 的挂起路径依赖模型主动 ask_user,建议后续给该
  子测试换一个必然追问的任务书(登记,不阻塞);
- 跨文件同名冲突当前由目录 Add 的冲突检测兜底,错误文案未特化
  (登记为体验优化)。

## 8. 复跑指令

```bash
go test ./... && go test ./skill/ ./runtime/loop/ ./config/ -race
MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) SMOKE_LIVE=1 \
  go test ./config/ -run 'TestLiveSmoke|TestLiveInlineProcedureAB|TestLiveDeliverReference|TestLiveDeliverFidelityAB|TestLiveDelegateParallelScan|TestLiveNoReAskAcrossTurns' -v -count=1 -timeout 60m
MINIMAX_API_KEY=... go run ./examples/pipeline
MINIMAX_API_KEY=... go run ./examples/interactive   # 交互场景
```
