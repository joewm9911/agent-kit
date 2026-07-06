# 依赖注入 / OO 审查 + store 注入修复方案

> **状态:已全部落地。** P1/P2 按 §4 实施(Todo 对象化、NewResultStore 收参、
> config resolveKV 注入),验收测试见 `config/di_isolation_test.go`(两个 agent
> 各配各的 todo/result 后端,断言互不串);P3 以 `observe.InstallTrajectory`
> (按 path 幂等)落地装配幂等兜底;P4 复查确认全部 `slog.Default()` 都在
> 构造函数边界、logger 统一从 `BuildOptions.Logger` 注入,符合约定,不改。
> 后续调整:builtin 按能力拆分并提为顶层包 `todo/`(`todo.New(kv, ttl)`)与
> `askuser/`(`askuser.New()`),builtin 目录删除;文中 `builtin.NewTodo` 即现在
> 的 `todo.New`。cap 引用 `cap://tool/builtin/ask_user` 不变,配置零迁移。
> suspend 的持久化协议也已收敛到 `store.KV`(kind 并入键前缀):`suspend.Store`
> 接口删除,Journal/恢复入口持有注入的 KV;file 后端泛化为注册表后端
> `impl/store/file`(type=file,todo/digest/suspend 皆可用),`suspend.dir` 是
> `store: file` 的简写,换 redis 即得多副本挂起恢复。

> 原则(本次基准):**消费方依赖抽象,不依赖具体实现,也不依赖全局。**
> 一个用到 store 的结构体应当**持有** store(收敛得了持有统一 Store 接口,
> 收敛不了持有该模块的 Store 接口),具体后端**由配置构造并注入**;消费方
> 既不感知 `inmemoryStore` 这种具体实现,也不从包级全局里去够。
> 构造(谁 new)与使用(谁持有)分离。

## 0. 收敛判断:四类 store → 三个接口(判断成立)

| 模块 | 操作形状 | 接口 | 收敛 |
|---|---|---|---|
| todo | 计划状态:键 → 一个 JSON 值 | `store.KV` | ✅ 与 digest 收敛到 KV 原语 |
| digest/result | 大结果暂存:键 → 值 + seq | `store.KV` | ✅ 同上,各自一层薄 adapter |
| session | Append/Load/Clear(消息日志) | `session.Store` | ✅ 与 KV 形状不同,独立接口 |
| memory | Put/Search(带 scope) | `memory.Store` | ✅ 独立接口 |

3 个接口服务 4 个模块,收敛本身正确。问题不在接口数量,在**持有与注入**。

## 1. 全局审查结果一览

| # | 位置 | 问题 | 级别 | 处置 |
|---|---|---|---|---|
| **P1** | `builtin.todoKV`+`SetStore`、`loop.resultKV`+`SetResultBackend` | 配置解析出的后端被推进**进程级可变全局单例**,消费方读全局而非持有 | 🔴 高(含正确性 bug) | **本方案修复** |
| **P2** | `todoKV = store.NewInMemory()`、`resultKV = store.NewInMemory()` | 消费方(builtin/loop)**硬编码具体默认实现** | 🟡 中(P1 症状) | 随 P1 一并消除 |
| P3 | `callbacks.AppendGlobalHandlers`(config.Build/BuildApp、observe.Install) | 可观测 handler 挂到 eino **进程级全局**,跨 app/跨 agent 共享、测试里累积 | 🟡 中(eino 施加) | **已修**(两轮):库内全局删除,幂等账本上移装配层 config/observe.go,按配置值去重 |
| P4 | 各处 `slog.Default()` 兜底(serving/source/config/observe/channel) | 未注入 logger 时够全局默认 logger;注入不一致 | 🟢 低(约定俗成) | **已复查**:均为构造边界兜底,logger 统一注入,合规 |
| ✅ 非问题 | source/store/session/memory/model/channel/prompt/engine/exec/vectorstore 的 `init` 注册表 | 全局 map 但**写在 init、只读于 New()**,是插件扩展机制(对齐 `database/sql`) | — | 保持 |

**要点**:注册表(`RegisterBackend` 等)是 append-once 的扩展接缝,**不是**本次要治的"运行期可变单例"。真正违背原则的是 **P1**——todo/digest 把每个 agent 的配置写进一个共享全局。

## 2. P1 详解:为什么是设计问题,也是正确性 bug

- [builtin/todo.go:55](../builtin/todo.go) `var todoKV store.KV = store.NewInMemory()` + [SetStore](../builtin/todo.go);todo 能力直接读全局 `todoKV`。
- [loop/digest.go:31](../loop/digest.go) `var resultKV store.KV` + [SetResultBackend](../loop/digest.go);`NewResultStore()` 绑全局。
- config 里 [wireGlobalStore(...)](../config/agent.go) 把每个 agent 解析出的 store **推进这两个全局**。

后果:
1. **多 agent 正确性 bug**:一个进程装配多个 agent 时,后一次 `SetStore`/`SetResultBackend` **覆盖**前一次 → 所有 agent 共用**最后装配的**那个 todo/result 后端,各 agent 的 `todo.store`/`digest.store` 配置对除最后一个外**静默失效**。
2. **消费方不持有依赖**:todo 工具 / digest 的行为取决于"全局被谁最后 set",而非它被"用什么构造"——依赖倒置被破坏。
3. **不对称**:session/memory 是每 agent 注入(agent 持有 `session.Store`,memory 能力持有 `memory.Store`),todo/digest 却走全局。同样"四大模块",两套机制。
4. **测试要全局复位**(`builtin.SetStore(store.NewInMemory(),0)`)——顺序相关、不隔离。

## 3. 对照:session/memory 已是正确模板

- **session**:[agent.Options.Store session.Store](../agent/agent.go),config 用 `session.New(...)` 构造后 [注入 agent.New](../config/agent.go)。agent **持有接口**,构造来自配置,不碰全局。✅
- **memory**:config 构造 `memory.Store` 注入 `memory.AsCapabilities` + `autoRecall`。能力持有接口。✅

todo/digest 向它们看齐即可。

## 4. 修复方案(P1/P2)

### 4.1 todo —— 引入持有 store 的 `Todo` 对象
`builtin` 里新增持有型对象,能力/提醒/清理都挂在它上,内部用 `t.kv`:
```go
// builtin/todo.go
type Todo struct { kv store.KV; ttl time.Duration }
func NewTodo(kv store.KV, ttl time.Duration) *Todo { return &Todo{kv, ttl} }
func (t *Todo) Capabilities() []capability.Capability { /* 闭包捕获 t.kv,不读全局 */ }
func (t *Todo) Nudge(caps []capability.Capability) []capability.Capability { ... }
func (t *Todo) Clear(ctx context.Context) { t.kv.Delete(ctx, sessionKey(ctx)) }
```
删 `todoKV`、`todoTTL`、`SetStore`;`loadState`/`saveState`/`NudgeTools`/`ClearCurrent` 收编为 `*Todo` 的方法(或显式收 `kv` 参数)。

### 4.2 digest —— `NewResultStore` 收 kv,agent 持有
```go
// loop/digest.go
func NewResultStore(kv store.KV, ttl time.Duration) *ResultStore { return &ResultStore{kv: kv, ttl: ttl} }
```
删 `resultKV`、`resultTTL`、`SetResultBackend`。agent 经 `Options` 持有
`ResultKV store.KV` + `ResultTTL`,`agent.go` 里
`loop.WithResultStore(ctx, loop.NewResultStore(a.resultKV, a.resultTTL))`。
未配置时由 config 传入一个默认 KV(经 `store.NewBackend("", nil)` 工厂解析,
**不在 loop 里 new**)。

### 4.3 config —— 解析后注入,不再 set 全局
`wireGlobalStore(...)`(set 全局)→ 改为 `resolveKV(ref, stores, kind) (store.KV, ttl)`,
然后:
- todo:`todo := builtin.NewTodo(kv, ttl)`;`caps = append(caps, todo.Capabilities()...)`;
  `caps = todo.Nudge(caps)`;主循环结束 `todo.Clear(ctx)`。
- digest:把 `kv, ttl` 传进 `agent.New(..., Options{ResultKV: kv, ResultTTL: ttl})`。

结果:**四类 store 统一**——消费方持有自己的 Store,config 负责构造,零运行期全局,每 agent 隔离;顺带修掉 P1 的 multi-agent bug,消除 P2 的硬编码默认。

### 4.4 实施步骤
1. builtin:`Todo` 对象化,删全局 + `SetStore`;`todo_test`/`nudge_test` 改用 `NewTodo(kv,...)`。
2. loop:`NewResultStore(kv,ttl)`,删全局 + `SetResultBackend`;`digest_test` 改造。
3. agent:`Options` 加 `ResultKV/ResultTTL`,`agent.go` 两处 `NewResultStore(...)` 传入。
4. config:`wireGlobalStore` → `resolveKV`,todo/digest 改注入;删对全局 setter 的调用。
5. 全仓 `go build ./... && go test -race ./...`;补一个**多 agent 隔离**测试(两个 agent 配不同 todo/result 后端,断言互不串)——这是 P1 修复的验收。

## 5. 次级项(P3/P4)——已处置

- **P3 全局回调(已修兜底)**:trajectory 的 `AppendGlobalHandlers` 原先每次
  Build 都追加——多 app 装配、副本重启测试(`loadStressApp` 两次)会累积
  handler、重复打开文件、轨迹重复记录,与 P1 同形。现收口为
  [observe.InstallTrajectory(path)](../observe/trajectory.go):按 path 幂等,
  重复装配不再累积;`observe.Install` 原有 `sync.Once`。剩余边界:eino 全局
  回调是进程级切面,多 app 不同 path 各记全量(无法按 app 过滤),根治要走
  eino per-invocation callback,属独立议题。
- **P4 slog 全局兜底(复查通过)**:所有 `slog.Default()` 都在构造函数边界
  (`logger == nil` 时),logger 从 `BuildOptions.Logger` 统一注入并向下传递
  (catalog/namespace/agent/serving/dispatcher),无深层全局读。这是 Go 标准
  惯例(对齐 `http.Server.ErrorLog`),保留。

## 6. 结论

- **P1/P2 已落地**:todo/digest 从"全局单例 + SetStore"改成"消费方持有、config 注入",与 session/memory 对称,修掉 multi-agent last-writer-wins bug;验收测试 `config/di_isolation_test.go` 用记录型后端断言两个 agent 的计划/大结果各落各的后端。
- **P3 已加装配幂等兜底,P4 复查合规**(见 §5)。
- 全项目**没有其他运行期可变全局单例**;注册表是合规扩展机制,不动。避免"后期问题非常多"的正是守住这条线:**注册表只读、运行期状态靠注入或 ctx**。
