# agent-kit 架构重组方案:OO 全量复盘 + 分层目录重组

> 输入:三路独立审查(全局状态与生命周期 / 封装与抽象依赖 / 包内聚与结构),
> 每条发现都核对过源码位置。本方案两部分:**§1-2 违规清单**(修什么),
> **§3-6 分层重组**(目录怎么摆),两者一次迁移中一起落地。

---

## 0. 现状一句话

24 个顶层目录平铺,层次靠脑补;协议/实现分离(三层 layering)与 store 注入
(DI 治理)两轮之后,**核心骨架是健康的**——依赖方向无环、注册表机制统一、
消费方持 store 接口。剩余问题集中在:**边界接口缺失(依赖具体类型)、
observe 的进程级全局、少数协议包仍内嵌实现、运行态 map 无生命周期**,
以及**目录不表达层次**。

## 1. OO 违规复盘(按严重度)

### 🔴 A 级:依赖具体类型,破坏可替换性

| # | 位置 | 问题 | 修法 |
|---|------|------|------|
| A1 | [serving/serving.go](../serving/serving.go)(`agents map[string]*agent.Agent`)、[channel/dispatcher.go](../channel/dispatcher.go)(`Binding.Agent *agent.Agent`) | 服务层依赖 agent **具体 struct**,无法注入 mock/代理/装饰过的 agent | 消费方定义最小接口 `Runnable`(Run/Stream/Name/Interrupt/Steer),serving 与 dispatcher 只持接口;`*agent.Agent` 天然实现 |
| A2 | [skill/skill.go](../skill/skill.go) `Deps.Catalog *source.Catalog`、`Deps.Prompts *prompt.Resolver` | skill 作为消费方持**具体类型**,测试必须造真 Catalog/Resolver | Deps 收敛为消费方小接口:`Selector`(Select(include,exclude))与 `PromptSource`(Resolve);Catalog/Resolver 天然实现 |
| A3 | [registry/](../registry/registry.go) 包名是角色泛名 | 唯一违反"包名表功能"的存留:装模型工厂却叫 registry,与 impl/**model**/* 不对应 | 更名为 `model` 协议包(接口+注册表+BuildModel),与 session/memory/prompt 对称。消费处与 eino 的 `components/model` 冲突时按既有惯例别名(`einomodel`) |

### 🟠 B 级:协议包内嵌实现 / 进程级全局

| # | 位置 | 问题 | 修法 |
|---|------|------|------|
| B1 | [secrets/secrets.go](../secrets/secrets.go) `Env`、`NewFile` | 与 session/memory 当年同形的封装泄漏:协议包内嵌两个实现,config 直接 `switch provider` 构造 | 协议化:`Provider` 接口 + 注册表;`impl/secrets/env`、`impl/secrets/file` init 注册;config 走工厂。(env 可比照 store.InMemory 留作零依赖默认,二选一,见 §6 决策点) |
| B2 | [observe/observe.go:70](../observe/observe.go) `installOnce`、[observe/trajectory.go](../observe/trajectory.go) `trajInstalled` map | 库替应用管进程级幂等:后到的 logger 被 Once **静默吞掉**;trajInstalled 是包级可变 map。上轮 P3 的幂等是止血,不是根治 | 幂等责任上移装配层:observe 只提供 `Handler(logger)`/`Trajectory(path)` 纯构造;config 持有"本进程已装观测"状态(App 级字段或装配器对象),库内两个全局删除 |
| B3 | [loop/budget.go](../loop/budget.go) `BudgetGate.sessions`、[loop/policy.go](../loop/policy.go) `ApprovalState.remembered` | 会话级运行态放进程内 map:**无过期**(泄漏,>4096 粗暴全清丢有效决策);多副本不同步——与 todo 当年"进程内 map"同形 | 两步:先 LRU+TTL 止血;终态可选落 `store.KV`(带 TTL、多副本一致),经 Options 注入,与 todo/digest 模式对齐 |
| B4 | [serving/serving.go:96,204](../serving/serving.go) `time.Now().UnixNano()` 造 session id、[suspend/suspend.go:36](../suspend/suspend.go) `rand.Read` | 时间/随机直接调用:不可 mock、纳秒并发可碰撞 | serving 复用 `suspend.NewTurnID()`(时间+随机);NewTurnID 保持简单但把 id 生成函数做成可注入变量(测试可覆盖) |

### 🟡 C 级:记录在案,不动(务实边界)

- **agent 持 `*loop.ApprovalState`/`*loop.BudgetGate` 具体类型**:它们是状态容器
  而非策略接口,无替换需求,字段已私有、构造经 config——保持。
- **`engine.Runner` 接口定义在提供方**:多消费方(agent/skill/loop)共享,定义
  在提供方是 Go 可接受形态——保持。
- **`store.InMemory` 内聚协议包**:零依赖缺省 + 语义参照,已文档化例外——保持。
- **engine.Assembly / loop 导出面偏宽**:装配 API,收紧属完美主义——保持。
- **全部注册表(init+Register+运行期只读)**:合规扩展机制——保持。
- **runctx 的 ctx keys**:全部是请求域瞬态(身份/交互通道/执行域/审批模式),
  符合 context 最佳实践——保持。

## 2. 结构审查结论(重组的事实依据)

- **loop(2107 行,14 文件)不是杂物抽屉**:11 种 Ring 0 治理职责
  (审批/预算/消化/压缩/重试/超时/截断/中断/轨迹/结构化/提示词分层)全部作用于
  同一边界(模型/工具调用点)、共享 runctx 键空间——**内聚正确,不拆**。
  问题只在名字:"loop" 不表达"治理中间件"。
- **engine(1520 行)内聚正确**:注册表 + 6 引擎 + graph 执行器共享
  Assembly/Runner 契约——不拆。
- **config(2036 行)分工清晰**:schema/装配/画像六个文件职责明确——不拆。
- **channel 包一包两责**:`Channel` 协议(接口+注册表,~100 行)+ `Dispatcher`
  会话分发(~330 行,依赖 agent/suspend/store)。协议归协议层,dispatcher
  归服务层——**要拆**。
- **扇入极值**:capability(56)与 runctx(37)是全项目地基;config 扇入 1
  (纯装配入口)。分组必须让这个层次可见。

## 3. 分层模型(重组的骨架)

依赖拓扑实测五层,自下而上(只允许上层依赖下层,同层内按注释白名单):

```
L0 地基     capability  runctx                    (零内部依赖,人人可用)
L1 协议     model(←registry) session memory prompt source channel(协议) store
            exec vectorstore secrets              (接口+注册表+工厂,零实现)
L2 运行时   engine  loop  suspend  observe        (执行引擎 + Ring 0 治理 + 挂起 + 观测)
L3 领域     agent  skill  todo  askuser           (领域核心与内置能力)
L4 服务     serving(+dispatcher)                  (HTTP gateway + IM 分发)
L5 装配     config  std                           (唯一许可的全量耦合点)
──────────  impl/<协议>/<实现>                    (平行:只依赖 L0/L1,init 注册)
            internal/testmodel                    (测试工具)
```

## 4. 目标目录结构(推荐方案)

顶层 24 → **12**,分组名 = 层次语义,组内包名仍是功能名(不违背"包名表功能"
——provider/builtin 那类是**拿角色当包名**,这里分组目录是命名空间,包名不变):

```
agent-kit/
├── core/                    # L0 地基
│   ├── capability/          #   能力抽象、CapRef、Risk、Params、Duration
│   └── runctx/              #   每轮运行上下文(身份/交互/执行域/fork)
│
├── protocol/                # L1 可插拔协议(每包 = 接口+注册表+工厂,零实现)
│   ├── model/               #   ← registry 更名迁入(A3)
│   ├── session/             #   会话短期记忆
│   ├── memory/              #   长期记忆
│   ├── store/               #   KV 原语(inmemory 默认随包,已文档化例外)
│   ├── prompt/              #   提示词源
│   ├── source/              #   能力供给源(含 Catalog)
│   ├── channel/             #   IM 通道协议(仅 Channel/Inbound/Outbound + 注册表)
│   ├── exec/                #   脚本执行引擎
│   ├── vectorstore/         #   向量库后端
│   └── secrets/             #   凭证(B1:Env/File 迁出)
│
├── runtime/                 # L2 运行时机制
│   ├── engine/              #   执行引擎族(react/plan-execute/graph/...)
│   ├── loop/                #   Ring 0 治理中间件(已决策:不更名)
│   ├── suspend/             #   挂起/恢复(卸载重放,落 store.KV)
│   └── observe/             #   观测(B2:去全局,纯构造)
│
├── agent/                   # L3 领域核心(门面,留顶层)
├── skill/                   # L3 领域核心(门面,留顶层)
├── todo/                    # L3 内置能力(维持上轮决策,留顶层)
├── askuser/                 # L3 内置能力(留顶层)
│
├── serving/                 # L4 对外服务:HTTP gateway + dispatcher(从 channel 迁入)
│
├── config/                  # L5 装配(schema + Build/BuildApp + profile)
├── std/                     # L5 默认后端聚合(空导入)
│
├── impl/                    # 官方实现(结构不变;impl/secrets/{env,file} 新增)
├── internal/                # 测试工具(testmodel)
├── examples/  docs/
```

设计取舍说明:

1. **agent/skill/todo/askuser/serving/config 留顶层**:它们是使用者第一眼要找
   的门面与领域词汇;分组的对象是"多而小、按机制聚类"的包。
2. **capability/runctx 进 core 而非留顶层**:它们是被 56/37 个位置引用的地基,
   收进 core/ 正是把"这是地基"写进 import path
   (`agent-kit/core/capability` 自解释)。
3. **store 归 protocol**:它有注册表、有 impl/store/file 与 redis 实现,本质
   是协议;"原语"只是它的形状描述。
4. **channel 拆分**是本方案唯一的包级拆分:协议(接口+注册表+各 IM 实现的
   注册点)留 protocol/channel;Dispatcher/Binding(依赖 agent 的服务逻辑)
   迁入 serving——顺手消解了"L1 channel 依赖 L3 agent"的层次倒挂,
   配合 A1 的 `Runnable` 接口,serving 对 agent 也只剩接口依赖。
5. **impl/ 完全不动**:三层 layering 约定(L1 不 import impl、init 自注册、
   std 聚合)原样保留,只是 L1 的 import path 前缀变化。

## 5. OO 修复 × 目录迁移的执行序

一次分支内完成,按批推进,每批 `go build ./... && go test -race ./...` 全绿:

| 批 | 内容 | 涉及 |
|---|------|------|
| 1 | **A1**:serving/channel 定义 `Runnable` 小接口,替换 `*agent.Agent` 字段 | serving、channel、config |
| 2 | **A2**:skill.Deps 收敛为 `Selector`/`PromptSource` 小接口 | skill、config |
| 3 | **A3**:registry → protocol/model(先在原地更名包,验证后随批 6 迁移) | registry、config、skill、impl/model/* |
| 4 | **B1**:secrets 协议化,Env/File → impl/secrets/{env,file},std 聚合 | secrets、config、std |
| 5 | **B2**:observe 去全局(删 installOnce/trajInstalled,幂等上移 config);**B4**:serving 复用 NewTurnID | observe、config、serving、suspend |
| 6 | **目录大迁移**:`git mv` 按 §4 就位 + channel 拆分 + 全仓 import 重写(脚本化 sed)+ gofmt | 全仓 |
| 7 | **B3**:BudgetGate/ApprovalState 加 LRU+TTL(store.KV 化单独立项,不阻塞本方案) | loop |
| 8 | **守卫落地**:分层依赖检查脚本(见 §7)进 docs/,冒烟 + 压测回归 | — |

批 6 是纯机械搬迁(包名除 registry→model 外全部不变,只改 import path),
风险集中在一次提交,可整体 revert;批 1-5 是行为级修复,先行独立验证。

## 6. 决策点(已拍板,2026-07-05)

| 决策 | 结论 |
|------|------|
| loop 是否更名 | **不更名**,保留 `loop`(runtime/loop) |
| secrets 的 env 默认 | **Env 不下沉**,随协议包(比照 store.InMemory 例外);File 下沉 impl/secrets/file |
| protocol/model 包名冲突 | **保持 `model`**,消费处按既有惯例别名 `einomodel` |
| B3 终态 | **先 LRU+TTL 止血**;store.KV 化等真实多副本审批需求出现再立项 |

### 6.1 todo/askuser 为何留顶层(判据澄清)

严格按扇入/体量,这两个包(扇入 2/1,350/40 行)是分组候选;但顶层平铺的
判据不是扇入,而是**"是否属于框架的产品面/领域名词"**:todo 与 askuser 出现
在配置开关(`todo.enabled`)、cap 引用(`cap://tool/builtin/ask_user`)、提示
词纪律(todo_write 规约)中,是使用者词汇表的一部分,与 agent/skill 同属 L3。
本方案的顶层规则即:**顶层 = 领域与产品面(L3-L5),分组 = 机制(L0-L2)+
实现(impl)**。反向没有诚实归宿——`builtin/`/`ability/` 是刚清除的角色泛名,
`runtime/` 语义错误,`impl/` 机制错误(它们非注册表后端)。若内置能力将来
增至更多,再立功能语义的分组不迟。

## 7. 分层硬规则(迁移后长期守卫)

```
1. core      不 import 本项目任何包
2. protocol  只 import core
3. runtime   只 import core、protocol
4. L3(agent/skill/todo/askuser)只 import core、protocol、runtime、彼此
5. serving   在 4 的基础上 + L3;对 agent 只经 Runnable 接口
6. config/std 无限制(唯一装配点)
7. impl      只 import core、protocol(+ impl/utils);永不被 L1-L4 import
8. 任何包不得出现运行期可变包级状态;注册表只在 init 写
```

守卫脚本(CI/本地皆可):

```bash
# 例:runtime 不得向上依赖
go list -deps ./runtime/... | grep -E "agent-kit/(agent|skill|serving|config)$" && exit 1
# 例:L1-L4 不得依赖 impl
go list -deps $(go list ./... | grep -v "/impl/\|/std\|/examples") \
  | grep "agent-kit/impl/" && exit 1
```

## 8. 收益与成本

**收益**:目录第一眼即分层(24→12);import path 自证依赖方向;A1/A2 补上
最后两处具体类型依赖,serving/skill 可独立测试;registry 命名归位;observe
不再吞配置;channel 层次倒挂消除。

**成本**:一次全仓 import 重写(脚本化,~40 文件);外部消费者(examples 与
任何树外依赖)一次性迁移——模块未发 v1,语义化破坏成本最低的窗口就是现在。

**不做什么**:不拆 loop/engine/config(内聚已被证实);不动 impl 三层约定;
不为 C 级项引入接口(避免"为了 DI 而 DI"的过度抽象)。
