# CapRef 协议重构 — 待办方案（交接）

状态：方案已定稿，两个决策已锁死，**待动手**。仓库当前 HEAD 是 `80e8478`（RAG 降格为 vector source）之后。

## 决策（已锁）
- A：cap 引用替换的是**存储后端**。builtin 能力（todo_write/read、read_result）是框架内置工具、语义固定、不可 include 替换；只有它们背后的 **Store 可换**。
- B：整份做（清单 1→9）。分布式多副本是硬需求，不切一刀。

## 分布式驱动：四个可外置 Store
多副本 serving / 进程重启下，「A 实例写、B 实例读不到」会破坏一致性。以下内部状态全部要能换到外部后端（redis/qdrant/…），且互相独立可配：
| Store kind | 背后能力 | 家族 | 现状 | 需补 |
|---|---|---|---|---|
| session | 会话短期记忆 | KV(keyed list) | 已有注册表 | 可后迁到共享 KV |
| memory | 长期记忆 | 搜索(vector) | 已有注册表 | — |
| todo | harness 计划清单 | KV | 包级全局 map | 薄适配器→共享 KV |
| result | digest 大结果暂存 | KV | 内存 | 薄适配器→共享 KV |

KV 家族（todo/result/session）共用一个 `KV` 原语（原子 `Update`+TTL，见下）；搜索家族（memory/retriever）走 vector backend。

## 协议：4 段
`cap://<kind>/<domain>/<name>@<version>` — 去掉旧的 provider 段。
- kind=对象类别；domain=归属域（源名/builtin/存储用途）；name=实例名/能力名；version 可选。

## kind 二分（4+3）
- **可调用能力**（进 capabilities.include）：tool / skill / component / agent
- **可命名对象**（装配注入到能力的槽）：prompt / store / retriever
- **model 不是 cap kind**：保持内联 `ModelConfig` + `RegisterModel` 供给，从不被 cap ref 引用，按「ref 资格原则」不入 cap 体系（与被删的假 model-step 同理）。
- 收敛：旧 retriever 能力→tool；旧 memory kind→工具归 tool、存储归 store；旧 flow→skill；component 升为正式 kind。
- **provider + resolver 四件套**：tool / prompt / store / retriever 四个 kind 都按「provider 注册表（按 type 供给）+ resolver（cap:// ref→对象）」两段实现。供给侧各 kind 保留自有注册表（不搞泛型）；引用侧统一走 Resolver 门面。skill/component/agent 供给、model 内联均维持现状。

## 实例配置 vs 引用（agent 层声明，私有作用域）
```yaml
# agents/assistant.yaml
stores:
  - {name: sess,  kind: session, type: redis,  config: {addr: ...}}
  - {name: ltm,   kind: memory,  type: qdrant, config: {...}}
  - {name: plans, kind: todo,    type: redis,  config: {addr: ...}}
  - {name: cache, kind: result,  type: redis,  config: {addr: ...}}
retrievers:
  - {name: my-vec, kind: session, type: vector, config: {...}}

model: {provider: minimax, config: {...}}   # 内联，不进 cap 体系（维持现状）

# 四个模块块：策略内联 + store 是 cap ref
session:
  store:  cap://store/session/sess
  recall: {retriever: cap://retriever/session/my-vec, top_k: 5, window: 20}
  compaction: {prompt: ..., threshold: 12000}
  include_tool_trace: true
memory:
  store:  cap://store/memory/ltm
  scope:  {write: user, read: [user, shared]}
  recall: {top_k: 3}
todo:
  store:  cap://store/todo/plans
  inject: per_round
  nudge:  true
digest:
  store:       cap://store/result/cache
  threshold:   4096
  summary_len: 300
```
- **每个模块块 = 策略内联 + 一个 `store:` cap ref**。策略（召回/压缩/作用域/校验/注入/nudge/阈值/摘要）归各自模块块，不再散落。
- 实现（redis/qdrant/vector）是实例配置的 type，不进 ref。
- 替换后端=声明另一实例、模块 `store:` 指过去；跨 agent 共享=各自 `store:` 指同一实例。
- 缺省简写 `session.store: file`（匿名实例+就地引用）保留，存量零迁移。
- Ring 0 边界：cap 只换存储/召回后端；todo 纪律(校验/注入/nudge)、digest 织入、session 织入、memory 作用域隔离是装配切面，随模块块配、不可替换。

## 对照表（旧→新）
- tool: `cap://tool.mcp/fs/read_file` → `cap://tool/fs/read_file`
- vector: `cap://retriever.vector/kb/x` → `cap://tool/kb/x`
- builtin: `cap://tool.builtin/context/read_result` → `cap://tool/builtin/read_result`
- skill: `cap://skill.graph/catalog/price-review` → `cap://skill/catalog/price-review`（引擎名消失）
- component: 无正式 ref → `cap://component/catalog/price_analyst`
- agent: `cap://agent.local/agents/x` → `cap://agent/agents/x`
- workflow: `cap://flow.workflow/workflows/x` → `cap://skill/workflows/x`
- prompt: `cap://prompt.file/pp/x@prod` → `cap://prompt/pp/x@prod`
- store 实例(新): `cap://store/session/sess`、`cap://store/memory/ltm`、`cap://store/todo/plans`、`cap://store/result/cache`；retriever 实例(新): `cap://retriever/session/my-vec`
- model: 维持内联 `ModelConfig`，不进 cap 体系；假 model-step `cap://model.step/internal/model` 删除
- read_result 工具本体仍是 `cap://tool/builtin/read_result`（由 digest 开关注入，不进 include）；它读写的后端由 `digest.store: cap://store/result/cache` 指定

## 短路径=cap:// 的糖
- `tools/<源>/<名>` ≡ `cap://tool/<源>/<名>`（kind 写死 `tool`，不再通配）
- `components/<名>` ≡ `cap://component/<本ns>/<名>`
- `tools/<源>/*` ≡ `cap://tool/<源>/*`（整源引入，name 通配保留）
- 两解析器(resolveToolFace/stepResolver)合流为"糖→具体 cap://→统一 Match"；`toolPattern` 产出具体 kind，旧 `cap://*.*/x/y` 作废。

## builtin 消灭假 source
`builtin`/`context`/`internal`/`step` 并入 `builtin`。builtin 能力都是 `cap://tool/builtin/*`，仍由开关注入不进 include。

## 内部抽象（对外 cap:// 不变，问题全压到内部）

### 四条不变式（写进协议定义，解决 #3/#5/#6 的「理解成本」，不改格式）
- **domain 不变式**：`domain` = 该 `kind` 下 `name` 的归属域；解析永远 **kind 优先**。callable→源/ns；store/retriever→slot-kind；prompt→prompt 源。因 kind 优先，`cap://store/session/x` 与 `cap://tool/session/x` 永不互比，「session 多义」消解。
- **通配不变式**：**kind 段永远精确**；domain/name 段可通配（`*` 与 name 前缀 `foo*`）。旧 `cap://*.*/x/y` 的 kind 通配是「短路径不知 provider」的残留——去 provider 后 `tools/x/y` 的 kind 必然是 `tool`，写死即可。kind 精确是 domain 不变式（kind 优先 dispatch）的前提：`*`-kind 无法选解析域。domain 仍可通配（「某 kind 全部」如 `cap://tool/*/*`）；「跨 kind 挂载全部」不走通配匹配，是 `Catalog.SelectAll` 的显式操作。
- **ref 资格原则**：只有能在配置里被命名/引用的才有 cap ref；纯内部 graph 节点 id 不配 ref。据此删掉假的 `cap://model.step/internal/model`（graph 内部 label）；model 从不被引用，同理不入 cap 体系（保持内联 ModelConfig）。
- **Key/version**：`Key()=kind/domain/name`（**不含 version**）。registry 存 `Key→{version→entry}`；ref 带 `@ver` 选版本、不带取 latest；priority shadowing 按 **(Key,version)** 比。仅 prompt 用 version，其余 version 空=唯一默认条目。

### Store 两层（解决 #1 原子性 / #2 生命周期）
- **下层 `KV`（driver，按 type 注册一次，全 KV 家族共享）**：`Get / Update(key, mutate, ttl) / Delete / Scan`。`Update` 是**原子读改写**原语（inmemory=mutex，redis=WATCH/Lua），TTL 焊进原语。唯一 backend 注册表。
- **上层模块 store（薄适配器，架在 KV 上）**：todo（list+stale 序列化进一个值，一次 `Update` 原子完成，nudge 走 `Update` 自增）、result（写一次读多次 + TTL）、session（keyed list，可后迁）。搜索家族（memory 长期记忆 / retriever）走**另一原语** vector backend（Upsert/Search，已存在），不并进 KV。
- 生命周期：删掉 `len>4096 清空整个 map` 的 hack；scope/session 结束走 `KV.Delete`，`retention` 配在模块块（`digest.retention`/`todo.retention`）由 TTL 兜底。

### Resolver 门面（解决 #4「统一 Match」的诚实措辞）
- 一个 `Resolver`，方法 `capability/store/retriever/prompt` 共用 `ParseRef+dispatchByKind`：callable→catalog(+shadowing)，store/retriever→agent 本地注册表，prompt→prompt 源。对外一套语法一套错误码，内部按 kind fan-out。
- **provider + resolver 对称**：tool/prompt/store/retriever 四个 kind 都是「provider 供给 → 登记 → resolver 按 ref 取」。链路 `type→Provider.New→实例→登记→cap:// ref→Resolver→实例`。model 不在此列（内联，无 ref）。

### 供给侧：各 kind 保留自有注册表，不搞泛型
tool/prompt/retriever 已各有「按 type 注册 provider」注册表（`source.Register`/`prompt.Register`/`session.RegisterRetriever`），store 新增 `store.RegisterBackend`（同款风格）。**这四个 kind 全按 provider+resolver 实现**，但供给侧维持各自注册表、不并成泛型 `Registry[T]`——泛型只给表皮一致，代价是大面积 churn + 抽象自重（Go 泛型仍是 N 个异构 typed 全局、无法跨 kind 枚举），ROI 不划算，否决。
- **model 维持现状**：内联 `ModelConfig` + 现有 `RegisterModel` 供给，不进 cap ref 体系、不加 resolver。
- skill/component/agent 供给维持现状；引擎保持闭合 switch（Ring 0 安全攸关，不开注册表，避免第三方引擎绕过治理）。
- 一致性的真收益在**引用侧统一**（Resolver 门面）+ 对外 cap:// 协议统一，不在供给侧泛型化。

## 进度（capref-refactor 分支）
- ✅ #1 KV 原语层（commit 4eb6574）
- ✅ #2 todo/result 适配 KV（commit 4eb6574）
- ✅ #3 ref.go 4 段 + 不变式（commit 35994c2）
- ✅ #4 全部构造点 + 假 source 并 builtin + 删 model-step（commit 35994c2）
- ✅ #5 component 升 kind（commit 待记）
- ✅ #6 解析器合流（commit 待记）
- ⬜ #7 stores:/retrievers: 具名实例 + 四模块块（**未做，见下方决策**）
- ⬜ #8 示例 store 块迁移（依赖 #7；ref 格式部分已随 #4 迁移）
- ⬜ #9 README

**#7 的性质与 #1-6 不同**：现有 config 已能内联配 session/long_term/retriever/digest 后端（memory.store 等），backend 仅注册了 inmemory（redis 未实现）。#7 是把这些重排成 `session:/memory:/todo:/digest:` 模块块 + `cap://store/` 引用层——**破坏性 config 改动 + 命名/共享糖，且无 redis 无法实测**。待与用户确认是否值得做。

## 实现清单（建议 commit 顺序）
1. **KV 原语层**：`store.KV` 接口（`Update` 原子 RMW + TTL）+ `store.RegisterBackend`（现有 session/retriever 注册表同款，不搞泛型）+ inmemory 后端 ~90。**验收：redis 语义的原子 `Update` + 多副本并发测试**（1、2 的关键，不可省）。
2. todo/result 两个薄适配器架到 KV 上：todo 去掉包级 map 与 4096 hack，result 抽出 ResultStore → KV — builtin/todo.go + loop/digest.go ~90（依赖 1）
3. ref.go 改 4 段 + 四条不变式落地（String/`Key`不含version/ParseRef/Match：kind 精确、通配仅 name 段）~50 — 地基
4. 16 处 Ref 构造点改格式 + 假 source 并 builtin + 删假 model-step ~45（依赖 3）
5. component 升 kind + comps 表正式化为 ns 本地目录（保留「只有 cap://skill 跨 ns」守卫）~60（依赖 3）
6. `Resolver` 门面：两解析器合流 + kind 分派 + store/retriever/prompt 解析统一入口 ~100（依赖 3；tool 路径产出具体 kind、无 `*.*` 通配对齐，回归面缩小；仍测试最厚：上表差异 + 跨 ns 守卫）
7. stores:/retrievers: 具名实例声明（含 todo/result）+ 四模块块（store cap ref + 策略字段）解析与槽接线 ~160（依赖 1,2,3,6）
8. 示例+smoke 迁移（含断言 ref 串的 e2e，爆炸半径偏大）~70
9. README 协议段重写（协议+三不变式+两层 store）~40
合计 ~715 行+测试。供给侧维持各自注册表（不搞泛型）；skill/component/agent/引擎/model 均不动。不兼容老协议（硬切）。对模型零影响（不见 ref）。
Ring 0 边界：cap 只换存储/召回后端；todo 纪律、digest 织入、session 织入、memory 作用域隔离 是装配切面，不可替换。

## 升级窗口（#8 硬切遗留）
审批决策记忆按 `ref.Key()` 存、挂起审批记录存 `ref.String()`（approval.go:133/146/159）。硬切格式会孤立**在途**的挂起会话/决策记忆 → 升级时清空在途挂起或写一次性迁移；稳态无影响。

## 单文件兼容路径注意
config.Load/Build（单文件形态）与 LoadApp/BuildApp（多文件）都要改；memory.KV 已带 scope，session.Retriever 注册表已存在（19、20 号任务已完成）。
