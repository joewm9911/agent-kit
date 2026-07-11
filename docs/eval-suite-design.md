# Eval 套件设计方案

> 状态:方案待 review。目标:把 agent 的"效果"从感觉变成**可复现的数字**
> ——任务成功率、grounding 率、工具准确率——从而让每一次提示词/引擎/工具
> 描述的改动都有客观裁决,而不是盲调。这是"效果驱动优化"的前提。

## 1. 为什么现有测试不够(先划清边界)

| 现有 | 测什么 | 不测什么 |
|---|---|---|
| smoke_e2e(testmodel 回放) | 管道通不通、编排接线对不对 | 真模型质量(回放脚本不是真决策) |
| engine_live_test(真机) | 引擎提示词的**格式遵循** | 任务是否**真的完成** |
| config/单测 | 装配、词法、fail fast | 一整条任务的端到端成功率 |

缺的是最要紧的那个数字:**"拿 N 个真实任务跑,agent 正确完成的比例是多少"**。
eval 套件专补这个,与上面三者正交、不替代。

## 2. 关键洞察:mock backend 自带 ground truth

eval 最难的是"怎么判成功"。agent-kit 运气好——examples 的 mock backend
(backend.go)埋了**已知答案的异常**:

| 实体 | 埋的事实(ground truth) |
|---|---|
| P103 | 热销但库存告急 → 补货候选 |
| P108 | 销量下滑 + 库存积压 → 清仓候选 |
| P115 | 成本 > 售价 → 亏本在售 |
| P117 | 连续 30 天零销量 → 滞销 |
| O-1042 | 已付款 12 天未发货 → 卡单 |
| O-1063 | 已完成但客户申请退款 |

于是"扫全店找亏本商品,agent 有没有找到 P115"是**机器可判**的——不靠模型
判分,靠对照 ground truth。这把 eval 的可靠性拉满,是这套方案能落地的地基。

## 3. 判分:三层,机器优先,LLM-judge 兜软指标

单一判分方式都不可靠(纯断言太脆、纯 LLM-judge 有方差)。分层:

**L1 机器硬判(决定性,零方差)——尽量多的成功判据落这里:**
- **工具调用事实**:该调的工具调了没(如退款任务是否调了 `query_order`+`apply_refund`)、参数对不对;
- **后端状态变更**:改动性任务(调价/发货/退款)执行后,查 mock backend 状态**真的变了**没;
- **grounding(反幻觉,杀手指标)**:抽取答案里的每个数字/ID/名称,验证它**都出现在某条工具结果里**——凭空捏造的数据直接判失败。这正是 L1 提示词里 "Data grounding" 那条纪律的**可度量化**;
- **命中率**:对照 ground truth,该找到的实体(P115/P117…)找到了几个。

**L2 LLM-judge(软指标,给 L1 判不了的):**
- 答案是否**切题**、是否给了可执行方案、口吻是否合适;
- 强模型 + 严格 rubric + 低温,每个维度 1-5 分;
- judge 本身也是一次模型调用,结果记 rationale 供人复核(judge 不是黑箱)。

**L3 过程指标(不判成败,记趋势):**
- token / 工具调用次数(成本代理)、延迟、是否用了 todo_write 拆计划、审批门有没有在该触发时触发。

**成功定义**:一次 run 通过 = L1 全部硬判过 **且** L2 judge 均分 ≥ 阈值。

## 4. 非确定性:跑 N 次,报**通过率**不报通过/失败

真模型有随机性。单次 pass/fail 没意义——一个任务 8/10 通过和 2/10 通过是天壤之别。

- 每个任务跑 N 次(core 套 N=3,full 套 N=5),报**通过率**;
- 汇总出**任务级通过率**和**套件总通过率**(headline 数字);
- 回归判定带**容差带**:非确定性下 ±X% 是噪声,跌破容差才算真回归(防止把随机波动误报成退化)。

## 5. 任务定义:声明式,复用 ops 场景 + backend

一个 eval 任务 = 声明式条目(YAML),复用现成的 ops-manager agent + mock backend:

```yaml
# evals/tasks/loss-making-scan.yaml
name: loss-making-scan
prompt: "扫一遍全店,哪些商品在亏本卖?逐个给处理方案。"
tags: [scan, multi-step, grounding]
runs: 3
success:
  tools_called: [search_products]          # L1:必须扫了商品
  ground_truth_hits: {must_include: [P115]} # L1:必须找到 P115
  grounding: strict                          # L1:答案数字全部可溯源
  judge_rubric: |                            # L2:软指标
    答案是否明确指出 P115 亏本、并给出可执行的处理建议(调价/下架)?
  judge_min: 4
```

改动性任务再加后端状态断言:

```yaml
# evals/tasks/refund-o1063.yaml
name: refund-o1063
prompt: "客户给订单 O-1063 申请退款,按售后政策处理。"
setup: {reset_backend: true}
success:
  tools_called: [query_order, apply_refund]
  backend_state: {order: O-1063, refunded: true}  # L1:退款真生效
  approval_fired: true                             # L1:危险操作过审批门
  grounding: strict
```

## 6. 分层套件 + 成本控制

真机 eval 花钱(N runs × M tasks × providers)。分档:

| 套件 | 规模 | 触发 | 用途 |
|---|---|---|---|
| **core** | ~12 任务 × 3 runs × 1 provider | 每次改模型面文本(提示词/引擎/工具描述) | PR 门:通过率跌破 floor 即挡 |
| **full** | ~50 任务 × 5 runs × 多 provider | 按需 / nightly | 全面画像 + 跨模型对照 |

`EVAL_LIVE=1` 门控(同 SMOKE_LIVE 纪律),key 走环境变量;`EVAL_TASKS=core|full`
选档;`EVAL_PROVIDER` 选模型。不开则整体 skip(CI 默认不烧钱)。

## 7. 报告 + 基线回归

- 每次运行产出结构化结果(JSON):逐任务通过率、逐维度(planning/tool-use/
  grounding/answer)细分、成本/延迟、judge rationale;
- 基线存档(committed baseline.json 或 CI artifact),新结果对照基线出**回归表**:
  哪些任务通过率掉了、掉多少、超没超容差;
- 人读的摘要:一屏看清"这次改动让核心套通过率 从 X% 到 Y%"。

## 8. 放置与分层

- 新增 `evals/`(与 `docs/` 平级,不进 import 图):任务 YAML + runner + judges;
- runner 复用 `config.LoadApp` 装配 ops-manager + mock backend,`EVAL_LIVE` 真机;
- judge 是一次独立模型调用(可与被测不同 provider,避免自评偏袒);
- 不进 `go test` 默认路径(是度量工具不是正确性测试),独立入口
  `go run ./evals` 或 `EVAL_LIVE=1 go test ./evals/`。

## 9. 实施批次

| 批 | 内容 | 规模 |
|---|---|---|
| 1 | 任务 schema + runner(装配 ops+backend、跑 N 次、收集工具调用/状态/答案/token) | 1 天 |
| 2 | L1 机器判分(工具调用、后端状态、grounding 抽取、ground-truth 命中) | 1 天 |
| 3 | L2 LLM-judge(rubric 打分 + rationale)+ 成功聚合 + 通过率 | 半天 |
| 4 | core 套 12 个任务用例(覆盖 QA/scan/改动/退款/审批/多步计划)+ 报告 | 1 天 |
| 5 | 基线存档 + 回归表 + 容差带;CI 接 core 套(EVAL_LIVE 门控)| 半天 |
| 6(后置) | full 套扩容 + 多 provider 对照 + nightly | 按需 |

批 1-4 先跑出"第一个通过率数字",这是整个"效果优化"的起点。

## 10. 需裁决

1. **judge 模型**:用被测同 provider(便宜)还是强模型独立评(更准、防自评偏袒)?推荐强模型独立评,core 套成本可控;
2. **core 套 floor**:PR 门的通过率下限设多少(如 80%)、容差带多宽(如 ±10%)——需先跑一轮拿到基线才好定,建议第一轮只记录不挡门;
3. **首批任务清单**:12 个 core 任务是否就用 ops 场景现成的六类异常 + 你手上真实高频问法?
4. **是否要人工标注集**:少量任务加人工"标准答案"做 judge 校准(验证 LLM-judge 和人判一致),推荐 core 套里挑 3-5 个做校准锚。
