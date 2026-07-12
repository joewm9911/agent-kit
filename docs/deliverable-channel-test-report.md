# 交付物直达通道测试报告

> 日期:2026-07-12;实现:155e33d..6f74a75 + 效果 A/B(本报告随附);
> 方案:docs/deliverable-channel-plan.md;验收标准:plan §8。
> 模型:MiniMax-M2.7(api.minimaxi.com);环境:macOS 本机 + 本地 redis。

## 1. 结论(TL;DR)

**全部通过。** 机制侧(捕获/存底/引用解析/随行)由确定性代码保证,
12 项单测 + 3 项集成全绿;模型行为侧真机 n=6 引用率 5/6、复述 1/6;
**效果 A/B:基线臂行保留 25.8/30(且出现一次 5/30 的摘要塌缩——正是
本通道要治的病),通道臂 6/6 全部 30/30、列 7/7,机制保证的 100% 保真**。
全量回归 34 包零失败,race 干净,分层守卫通过。

## 2. 测试矩阵

### 2.1 单元测试(确定性,12 项)

| 用例 | 验证点 | 结果 |
|---|---|---|
| TestDeliverCaptureAndMarker | 捕获入 sink、标记注入、标题启发、d1 可经 read_result 取回、证据类零污染 | PASS |
| TestDeliverDegradeWithoutStore | 后端缺席降级轮内 id(d0N),本轮随行不受影响 | PASS |
| TestDeliverNoSinkNoop | 出站方未装 sink = 完全 no-op(裸跑零成本) | PASS |
| TestDeliverSinkConcurrency | 20 并发捕获,id 唯一、计数准确 | PASS |
| TestDigestPreservesDeliverMarker | 超阈值交付物被消化后,#dN 标记回贴摘要头(引用链不断) | PASS |
| TestReadResultDirtyIDAndMissHint | 脏 id 归一化(" r1"/"R1"/"结果r1")+ miss 回报可用清单 | PASS |
| TestDirectDeliverable(4 子场景) | direct 四态:独占末次触发/后续有调用不触发/多 direct 不触发/attach 不触发 | PASS |
| TestResolveDeliverables | 引用出现序、always 追加、同 id 去重、幻觉引用忽略、零引用只随 always | PASS |
| TestResolveDeliverablesGuards | 数量护栏(7 引 5 留)、体量护栏(200KB 截断) | PASS |
| TestConfigEnumFailFast/deliver_enum | `deliver: attch` 装配期报错指路 | PASS |

### 2.2 集成测试(确定性)

| 用例 | 链路 | 结果 |
|---|---|---|
| TestDispatcherDeliverableFollowup | IM:终答引用 → KindDeliverable 随行消息,原文逐字节送达 fakeChannel | PASS |
| TestHTTPDeliverables | HTTP /messages:响应体 `deliverables` 数组带原文 | PASS |
| (既有全量回归) | 34 包全绿——通道为纯增量,存量行为零变化 | PASS |

### 2.3 模型行为(MiniMax 真机,TestLiveDeliverReference,n=6)

| 指标 | 结果 | 判读 |
|---|---|---|
| 引用率(终答含 #dN) | **5/6** | 达标(阈值 2/3);失败臂退化为复述,冗余不失真 |
| 复述率(终答 ≈ 原文长度) | **1/6** | 通道生效时终答仅 9-22 字符导读 |
| 附件逐字节一致 | **5/5**(引用轮次) | 机制保证,恒真 |

### 2.4 效果 A/B(MiniMax 真机,TestLiveDeliverFidelityAB,每臂 n=6)

尺子:用户可见文本(基线=终答;通道=终答+随行附件)中,30 个行锚点
(P001..P030)与 7 个列锚点的保留率。

| 臂 | 行保留(均值) | 列保留 | 最差单次 |
|---|---|---|---|
| 基线(无通道) | 25.8/30(86%) | 7/7 | **5/30(17%)——模型摘要塌缩** |
| 通道(attach) | **30/30(100%)** | **7/7** | 30/30 |

判读:1.5KB 的小报表下模型多数时候会照抄(基线 5/6 全保),但 **1/6
概率无预警塌缩到 17%**——这正是 interactive 实测和 insight-agent 反馈的
"信息缺失"病灶;真实业务报表(几十 KB)下塌缩是常态而非偶发。通道臂
的 100% 与内容长度无关,是机制保证。

### 2.5 系统级验收(interactive 真机,examples/interactive)

`sales-report` 声明 `deliver: attach` 后一轮真实对话:

```
✓ sales-report → [交付物#d1|sales-report] 已存底;终答中引用 #d1 …
· 模型 → "[交付物#d1|音频品类近30天销售报表] 总销量 2,724 件…整体结论…"(导读,未复述)
── 交付物 #d1 · 音频品类 30 天销售报表(cap://skill/sales/sales-report)──
# 音频品类 30 天销售报表(原文逐字节)
```

对照 plan §8 验收:①报表原文零损耗随行 ✓;②未标记/未引用能力零附件
(引用即附带)✓;③KV 故障降级不拉闸(单测)✓;④真机引用率/复述率/
一致性 ✓。

### 2.6 稳定性门禁

| 项 | 结果 |
|---|---|
| `go test ./...`(34 包) | 0 FAIL |
| `-race`(runctx/loop/serving/agent) | 干净 |
| scripts/layering-check.sh | 通过(core→protocol→runtime→L3→serving 无违约) |

## 3. 已知边界(如实)

- **引用率非 100%**:1/6 概率模型复述而非引用——退化形态是冗余(用户
  多看一遍),不是失真;`attach: always` 可完全消除对模型行为的依赖。
- **流式路径**:Stream 不做 direct 替换(token 已外发);随行呈现走
  dispatcher 的非流式收口,streamReply 快速路径暂无附件(文档已注明)。
- **合成型交付物不在范围**:大脑跨多源亲笔撰写的内容仍依赖模型质量,
  业界同样无解;本通道只保证"搬运型交付"零损耗。
- **跨轮引用**(v1 不做):历史交付物经 read_result(dN) 取回后本轮再引用。

## 4. 复跑指令

```bash
# 单测+集成(无外部依赖)
go test ./runtime/loop/ ./agent/ ./serving/ ./config/ -run \
  'TestDeliver|TestDigestPreserves|TestDirectDeliverable|TestResolve|TestDispatcherDeliverable|TestHTTPDeliverables|TestConfigEnumFailFast' -v

# 真机(需 MiniMax key)
MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
SMOKE_LIVE=1 go test ./config/ -run 'TestLiveDeliverReference|TestLiveDeliverFidelityAB' -v -count=1 -timeout 30m
```
