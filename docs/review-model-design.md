# ReviewModel:生成评审统一层(ShouldRetry 语义否决 × 守卫统一)

> 状态:方案 → 实施。借 eino ADK `ModelRetryConfig.ShouldRetry` 的核心洞察
> ——**网络重试与生成质量否决是同一个接缝**——把四个各自为战的质量守卫
> 收敛为"一个有界评审循环 + 有序评审器列表",并落地全局弹回预算与
> 改写输入重试两项新能力。

## 1. 背景与问题

当前主循环模型链(buildAgent):

```
RepeatBreak( CheckedFinish( FinishGuard( BudgetModel( RetryModel( raw )))))
```

四个质量守卫(FinishGuard/CheckedFinish/RepeatBreak/诚实标记)机制完全
同构——尝试→判定→注入纠正→有界重试→放弃兜底——但各自实现三方法接口
+ WithTools 透传 + 弹回循环(每个 ~80 行样板)。三个结构性问题:

1. **弹回预算乘法放大(真隐患)**:各守卫独立计数(2/2/1+强制),链式
   嵌套下一次逻辑调用最坏可触发 2×2×2 层层放大的内层生成,无全局闸。
2. **顺序语义隐式**:守卫先后由包装嵌套决定("RepeatBreak 必须最外"
   靠注释锁着),新守卫加错位置行为即错。
3. **能力缺口**:只能 append 纠正消息,不能**改写输入重试**(429 裁剪、
   上下文超限截短);厂商特定错误分类无法插拔。

## 2. 目标与非目标

**目标**
- 一个有界评审循环(全局重试预算)承载全部质量守卫;
- 守卫降级为纯函数评审器(Reviewer),顺序显式、新增 ~15 行;
- Verdict 支持 Append(纠正)/ Rewrite(改写输入)/ Force(强制收束)
  / Backoff(覆盖退避)——ShouldRetry 的完整语义;
- 既有全部守卫**行为不变**(现有行为测试原样通过是迁移验收线)。

**非目标**
- 不动 RetryModel/BudgetModel:瞬时错误重试"同参、不计费"与预算记账
  是另一层职责,保持在内层(评审重试经内层计费,与现状一致);
- 不动工具侧 Ring 0(审批/断路/超时/消化);
- 不新增配置面(预算为常量,YAGNI)。

## 3. 设计(逐条对照设计原则)

### 3.1 API(runtime/loop/review.go)

```go
// Attempt 是一次生成的完整上下文,交给评审器裁决。
type Attempt struct {
    N    int               // 第几次尝试(1 起)
    Msgs []*schema.Message // 本次实际发送的消息
    Out  *schema.Message   // Err == nil 时有效
    Err  error
    tally map[string]int   // 各评审器已触发次数(循环维护)
}
func (a Attempt) Tally(reason string) int // 评审器自限的依据(无状态化)

type VerdictAction int
const (
    Accept VerdictAction = iota // 放行(零值即默认)
    Retry                       // 重试:Append/Rewrite 之一 + 可选 Backoff
    Force                       // 强制收束:Replace 为最终输出,不再评审
)

type Verdict struct {
    Action  VerdictAction
    Reason  string            // 触发计数键 + 观测 span 标签
    Append  []*schema.Message // Retry:追加(弹回纠正的标准形态)
    Rewrite []*schema.Message // Retry:整体替换下次输入(改写重试)
    Replace *schema.Message   // Force:最终输出
    Backoff time.Duration     // Retry:覆盖等待(0 = 不等)
}

type Reviewer func(ctx context.Context, a Attempt) Verdict

// ReviewModel 有界评审循环:依序征询评审器,首个非 Accept 生效;
// 全局重试预算耗尽后放行(守卫是纠偏不是硬闸)。
func ReviewModel(m model.ToolCallingChatModel, rs ...Reviewer) model.ToolCallingChatModel
const reviewMaxRetries = 4 // 全局预算:每次逻辑调用最多 4 次评审重试
```

- **消费方持接口/不读全局**:Reviewer 一律经装配注入;循环不认识任何
  具体守卫。评审器需要的轮内状态走既有 runctx.TurnState(checked/denied
  的自节流现状不变)。
- **错误分两类**:Attempt.Err 暴露给评审器(为将来 429 改写重试留缝),
  但轮次终止级错误(TurnTerminal/中断)循环直接透传,不进评审。
- **纪律靠 harness 且有界**:全局预算 + 各评审器经 Tally 自限,双层有界;
  耗尽放行,不死循环。
- **观测**:每次评审重试经 observedGenerate 自报 span,Reason 即名字
  (review/<reason>),tracing 树里每次弹回可见。

### 3.2 评审器迁移映射(判定逻辑零改动,只换骨架)

| 现守卫 | 评审器 | 触发 | Verdict |
|---|---|---|---|
| RepeatBreak | RepeatBreakReviewer() | 输出只含热点调用 | 第 1 次 Retry{Append: out+tool 纠正};第 2 次 Force{Replace: 引用真实结果} |
| FinishGuard | FinishReviewer() | 无调用且 badFinal | Tally<2 → Retry{Append: out+system 纠正};否则 Force{Replace: 诚实标记后的原文} |
| CheckedFinish | CheckedReviewer(checks…) | 无调用且某 check 非空 | Retry{Append: out+system(check)}(check 自节流,现状) |

顺序(显式列表,原嵌套语义):`RepeatBreak → Finish → Checked`。
Force 直接返回不再评审(= 现状 RepeatBreak 最外、其强制收束不被再弹)。

### 3.3 守卫职责归位:从"模型链属性"迁到"循环装配"

原 FinishGuard 挂在 app/agent/decl/hub 的模型链上,被 direct 单发、
graph model 步等**非循环路径**连带穿上——那些路径输出即结果,弹回
无意义纯花钱。统一后:

- 模型链瘦身为 `BudgetModel(RetryModel(raw))`(4 处:app.go/config.go/
  agentModel/hubs.go + skill.buildDeclModel);
- **react 循环装配点**统一套 ReviewModel(守卫是循环的纪律,不是模型
  的属性):buildAgent 主循环(全家 + finishChecks)、skill.Build 与
  BuildPack 子循环(RepeatBreak+Finish+Checked(denied));
- engine 不 import loop(已知环),ReviewModel 由装配层(config/skill,
  均已依赖 loop)在传入 engine.Build 前套上——与现状 CheckedFinish/
  RepeatBreak 的接线位置一致。

**行为差异(如实登记)**:direct 引擎、graph 的 model 步、无工具退化
的 bareModelRunner 不再有 FinishGuard——单发模板调用,伪调用/空头承诺
守卫对其无纠正意义(无循环可继续);登记于 gap 文档。

### 3.4 兼容外观(迁移安全网)

`FinishGuard(m)` / `CheckedFinish(m, checks…)` / `RepeatBreak(m)` 保留为
薄外观 = `ReviewModel(m, 对应单个评审器)`——**现有全部守卫行为测试
一行不改、原样通过**,这是本次重构的验收线;判定函数(badFinal/
matchesHot/hotEntry/DeniedCallsCheck 及全部纠正文案)原文件原位不动。

## 4. 风险与回滚

- 风险:弹回语义细节走样(诚实标记时机、tool 消息协议回填)。对策:
  外观 + 既有测试原样通过;新增循环语义测试(全局预算封顶/Force 不再
  评审/Rewrite/Tally);真机一轮对照。
- 回滚:装配点单点替换,revert 单个 commit 即回旧链。
- 寿命:若未来迁 ADK,本层被 ModelRetryConfig/Middleware 取代——
  Reviewer 是纯函数,判定逻辑可直迁 ShouldRetry,折旧成本最小化。
