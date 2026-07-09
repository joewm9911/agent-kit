# brain-loop 升级 效果测试报告

> 日期:2026-07。范围:brain-loop-upgrades.md 已落地的变更。
> 诚实边界:**端到端任务成功率(eval)尚未建**,所以本报告不含"成功率提升
> X%"这类数字。测试分两类,各有其"效果"含义:
> - **进程内改进**(召回打分/片段/泄压阀/窗口):效果 = 注入 prompt 的内容
>   质量,**可直接断言**(确定性测试即效果验证,无需真机);
> - **改模型行为的项**(GoalCheck):效果 = 是否改变任务结果,**必须真机 A/B**。

## 1. 汇总表

| 变更 | 类型 | 测试 | 结果 |
|---|---|---|---|
| U1.2 近因加权 | 进程内 | TestRecencyWeighting | ✅ 更新的命中排前 |
| U1.3 Jaccard 归一 | 进程内 | TestSearch(过滤/排序不回退) | ✅ |
| U1.4a snippet-around-match | 进程内 | TestSnippetAroundMatch | ✅ 长消息中间的匹配点被片段覆盖 |
| U1.4b/c 长度可配/预算 | 进程内 | SearchOpts 路径 | ✅ |
| U1.1 指代近因兜底 | 进程内 | TestReferentialFallback | ✅ 指代 query 兜底带近因标注 |
| U1.4d include_tools | 进程内 | TestIncludeTools | ✅ 默认不召回执行记录,开启后可召回且限额 |
| U5.1 上下文泄压阀 | 进程内 | TestPressureCut | ✅ 超预算保留窗前移/无 MaxTokens 不介入/不越下限 |
| window↔keep_recent 解耦 | 进程内 | TestWindowKeepingHead | ✅ 小窗口下摘要+锚定存活/只裁近期原文 |
| U4.1 GoalReviewer | **改行为** | TestLiveGoalCheck(真机 A/B)+ 确定性 | ⚠️ 见下 |

全部确定性测试通过(bigram/todo/loop/agent 四包 ok),全库回归 + 分层守卫绿。

## 2. 进程内改进的"效果"验证

这类变更**不改模型行为**,只改注入 prompt 的召回内容/上下文形状。它们的
"效果"就是输出质量本身,可直接断言,不需要真机:

- **U1.4a snippet-around-match**(修隐性 bug):`TestSnippetAroundMatch` 构造
  一条前后各一大段无关文本、"P103 当前库存 42"埋在中间的长消息,断言召回
  片段**含"库存 42"**——证明片段取匹配点周围而非前缀(旧实现会截前缀、
  丢掉中间的匹配内容)。这是可直接测量的效果:召回从"定位对了却送错内容"
  变成"送对内容"。
- **U1.2 近因加权**:`TestRecencyWeighting` 两条词法命中相当、一新一旧,断言
  更新的排前——排序效果直接可测。
- **U1.1 指代近因兜底**:`TestReferentialFallback` 用"那个订单啥情况"(与目标
  轮次几乎零词法重叠),断言纯 bigram 会空、兜底补回近因轮次——补的正是
  bigram 结构上救不了的回指死角。
- **U1.4d include_tools**:`TestIncludeTools` 断言默认不召回执行记录、开启后
  可召回且单独限额——opt-in 语义与护栏可测。
- **U5.1 泄压阀 / 窗口解耦**:`TestPressureCut` / `TestWindowKeepingHead`
  直接断言机制(超预算前移、摘要恒保留),见各自测试。

**效果口径**:这些是"输出正确性/质量"层面的效果验证,**不是**"任务成功率"
层面——后者要 eval。但对进程内改进,输出质量即其全部效果(它不经过模型的
不确定性),所以确定性测试是充分的。

## 3. U4.1 GoalReviewer:真机 A/B(唯一改行为的项)

改模型行为,必须真机测。做了**干净的隔离 A/B**(`TestLiveGoalCheck`,
glm-5.2):两臂都开 todo,仅切换 `capabilities.goal_check`,隔离 GoalCheck 的
边际效应(不与整套 todo 机制混淆)。三部分易漏答任务(查库存/售价/退款),
机器判 grounding(三个数字都得出现)。

| A/B(todo 均开,仅切 GoalCheck) | 全覆盖率 | 模型调用数 |
|---|---|---|
| goal_check=true | 3/3 | 12(4×3) |
| goal_check=false | 3/3 | **9(3×3)** |

**结论:强模型 + 中等任务上,GoalCheck 零覆盖增益、+33% 调用成本——纯开销。**
另在首轮混淆实验(todo 开/关对比)观测到一次 0/3 退化(模型把"核对"答成
meta 陈述、丢了具体数字),说明强制全量重生成有丢内容的失败模式。

**处置:default-off**(`capabilities.goal_check`,默认关)。机制保留供弱模型/
更难/高风险任务显式开启;是否放开默认待 eval 系统量化(需"基线会失败"的
任务才测得出增益)。确定性接线由 `TestTodoGoalCheckWiring`(显式开)+
`TestGoalCheck`(提示内容/自限/跳过纯问答)覆盖。

## 4. 未做效果测试的项(诚实登记)

- **U2.1 拆解施压 / U3.2 完成证据检查**:**未实现**。数据(GoalCheck A/B +
  todo 开/关对比)一致指向"给强模型+中等任务加结构/纪律倾向净负",这两项
  属同类,default-on 预期有害;U3.2 核心还被 FinishReviewer 大部分覆盖。故
  按数据不建,登记待 eval。
- **U3.1 漂移提醒**:早已存在(todo.Nudge),无新增测试。

## 5. 报告的诚实边界

1. **没有任务成功率数字**——eval 套件未建,所以除 GoalCheck 外的项,我能证明
   "召回/上下文的输出更好",但**不能**证明"任务完成率上升 X%";
2. **GoalCheck 的 A/B 是 n=3、单模型、单任务**——样本小,结论"强模型上净开销"
   可信,但"在哪些任务分布上转正"要 eval 才能定位;
3. **进程内改进的上限**受召回本身的分量限制(前面评估过:200K 窗口下召回退居
   二线;召回内容还受截断/范围约束)——所以这些改进"方向对、幅度待测"。

**一句话**:改行为的项(GoalCheck)做了真机 A/B,结果是净开销 → 默认关;
进程内的项做了确定性效果测试(直接测召回/上下文输出),方向验证充分但
任务级幅度待 eval。这份报告如实反映了"从效果验证"能到的边界。
