# 单 agent 优先执行模式测试报告

> 日期:2026-07-13;方案:single-agent-mode-plan.md;模型:MiniMax-M2.7。
> 结论先行:**四组真机数据全部达标或超预期,形态成立**;批5 的工具面
> 顾虑被数据解除,批3 的委派观察出现"模型正确地不委派"的高质量信号。

## 1. 交付范围

| 批 | 内容 | 状态 |
|---|---|---|
| 批1 | mode: inline 过程卡 + 工具直挂 + 六项互斥校验 + Tag 豁免(digest/rewoo) | ✅ |
| 批2 | L1 过程卡纪律(tail 契约 + todo 跟踪句) | ✅ |
| 批3 | builtin delegate(治理:深度1/轮数收紧/并发闸/共账/scope) | ✅ |
| 批4 | digest.degrade_keep(暂存降级应急保留,缺省 24000) | ✅ |
| 批5 | 工具面阶梯 A/B(治理必要性实证) | ✅(顾虑解除) |
| 批6 | interactive 样板(price-check 过程卡 + ops-manager 开 delegate) | ✅ |
| 收窄 | skillpack trust: inline(外部包 inline 未开放,trust 键随其后置) | 登记 |

## 2. A/B 与真机数据

### 2.1 inline vs subloop(核心 A/B,n=6/臂)

同一四步定价审查流程,尺子 = 四锚点完成率 / 双工具贯彻率 / 模型调用数:

| 臂 | 完成 | 贯彻 | 模型调用均值 |
|---|---|---|---|
| subloop(基线) | 6/6 | 6/6 | 4.0 |
| **inline(过程卡)** | **6/6** | **6/6** | **3.0(-25%)** |

判读:质量持平、成本 -25%;担心的"拿到指引一句带过"零发生
(L1 "承认不执行=失败轮"生效)。**通过 plan §7 达标线,inline 放行。**

### 2.2 动态委派观察(n=3)

三品类扫描 + delegate 可用:任务完成 3/3(预埋异常全中),delegate
使用 0/3——模型三次都选择同轮 batch 直扫(工具在手、结果小,直扫
最优)。判读:**"正确地不委派"是决策框架生效的证据**,delegate 描述
里"适合并行线索/大中间数据"的边界被模型准确执行;委派行为的正向
用例(大数据隔离)留待真实场景数据,机制侧治理由单测锁住
(task 必填/未知工具点名/交互类剔除/终答透传)。

### 2.3 工具面阶梯(批5 尺子,n=6/档)

1 个目标工具埋进 N-1 个近义干扰工具,量首调命中率:

| N=8 | N=16 | N=32 |
|---|---|---|
| 6/6 | 6/6 | 6/6 |

判读:MiniMax 在 32 工具面下选择零衰减——interactive 规模(30+ 工具)
的工具面膨胀顾虑**解除**,描述瘦身降级为常规卫生而非阻塞项。

### 2.4 降级保留(批4,确定性单测)

暂存后端故障时:6000 rune 结果**零损失**进上下文;30000 rune 保留
24000 头 + 醒目披露;degrade_keep 可配。WARN 带 kept/total 可告警。

## 3. 稳定性

全量 go test 0 FAIL;-race(skill/loop/config)干净;layering 通过;
interactive app.yaml 严格解析通过(含新样板)。

## 4. 遗留与观察

- 外部 skillpack 的 inline(含 trust 分级)未开放——外部指令进主上下文
  是独立的安全决策,等 inline 在自有 skill 上积累数据后再议;
- 委派的正向场景(大中间数据)未在真机出现,建议接入 insight-agent
  真实任务后回采数据;
- compaction 长跑压测(40+ 调用)沿用 interactive 旗舰场景历史数据,
  未单独加压——inline 大范围铺开前建议补一轮。

## 5. 复跑指令

```bash
go test ./skill/ -run 'TestInline|TestDelegate' -v
go test ./runtime/loop/ -run TestDigestDegrade -v
MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) SMOKE_LIVE=1 \
  go test ./config/ -run 'TestLiveInlineProcedureAB|TestLiveDelegateParallelScan|TestLiveToolFaceLadder' -v -count=1 -timeout 30m
```
