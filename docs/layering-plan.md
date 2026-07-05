# 三层分离:协议 / agent-kit 实现(impl)/ 第三方

> 状态:**设计/盘点**,未落地。目标:把"可扩展协议"与"实现"拆干净,让基于
> agent-kit 的开发者清楚看到——哪些是协议(我可以自己实现)、哪些是 agent-kit
> 给的实现(我可以替换)、我自己的实现该怎么挂进去。

## 0. 三条命名/分层原则(本次基准)

1. **包名表功能,不用角色化泛名**。`provider/` 这种"提供者"是角色不是功能,
   废弃;每个包按它**干什么**命名(`redis`/`http`/`mcp`/`feishu`…)。
2. **agent-kit 提供的实现统一放 `impl/`**。`impl/` 是 L2 层根(层级标记),
   其下子包用功能名。
3. **第三方不可扩展的部分,遵循内聚**。只有真正的**第三方扩展接缝**才做
   协议/实现拆分;不是第三方后端的东西(框架内部机制、核心抽象)保持内聚,
   不为拆而拆。

## 1. 三层定义

| 层 | 是什么 | 落点 | 依赖 |
|---|---|---|---|
| **L1 协议** | 接口 + 注册表 + 解析器 + 中性类型,**零后端实现** | 功能名协议包 | 不依赖 L2/L3 |
| **L2 impl** | agent-kit 内置实现,`init` 自注册 | **`impl/<功能名>`** | → L1 |
| **L3 第三方** | 树外实现 L1 + 空导入 | 开发者包 | → L1 |

硬约束:**L1 永不 import L2/L3**(可加 lint 守)。

## 2. 现状盘点:15 个扩展点 + 内聚判定

用原则 3 先判"是不是第三方接缝":是→拆(L1+impl),否→内聚。

| # | 协议 | 现处 | agent-kit 实现/现处 | 第三方接缝? | 处置 |
|---|---|---|---|---|---|
| 1 | `capability.Capability` | `capability/` | 内置能力 `builtin/`;Ring0 `loop/` | **否**(用 `capability.New` 造,非注册表) | **内聚** |
| 2 | `source.Source` | `source/` | http/mcp/a2a/rpc/local/vector/exec | 是(最大接缝) | 拆 → `impl/*` |
| 3 | `store.KV` | `store/` | **inmemory** 同包;redis | 低层原语 | **内聚**(inmemory 留 L1;redis 寄 impl/session/redis) |
| 4 | `session.Store` | `session/` | **inmemory** 同包;redis | 是 | 拆 |
| 5 | `session.Retriever` | `session/recall.go` | **bigram** 同包 | 是 | 拆 |
| 6 | `memory.Store` | `memory/` | **inmemory** 同包;redis | 是 | 拆 |
| 7 | `engine.Runner`/`Builder` | `engine/` | **6 个内置引擎**同包 | **否**(框架执行策略;react=主循环) | **内聚** |
| 8 | `channel.Channel` | `channel/` | feishu `channel/feishu` | 是 | 拆 → `impl/feishu` |
| 9 | `prompt.Provider` | `prompt/` | **inline/file/http** 同包 | 是(对接提示词平台) | 拆 |
| 10 | `secrets.Provider` | `secrets/` | **Env/File** 同包 | 弱(可加 vault) | 拆(小) |
| 11 | `suspend.Store` | `suspend/` | **FileStore** 同包 | 弱(可加 redis) | 拆(小) |
| 12 | `runctx.Interactor` | `runctx/` | CLI `interact/` | 是 | → `impl/cli` |
| 13 | model `ModelFactory` | `registry/` | openai/minimax | 是 | → `impl/*` |
| 14 | `exectool.Engine` | **`provider/exectool`** | docker=范例 | 是 | **协议上浮** + 拆 |
| 15 | vector retriever backend | **`provider/vector`** | inmemory-lexical + 外部库 | 是 | **协议上浮** + 拆 |

三类毛病:
- **A 内置默认与协议同包**(3/4/5/6/9/10/11):协议包里塞了 agent-kit 的默认实现。
- **B 协议下沉到实现层**(14/15):`exectool.Engine`、vector 的 retriever 注册表
  长在 `provider/` 里,看着像实现细节,其实是第三方接缝。
- **C 泛名**:`provider/` 整层是角色化泛名。

**内聚保留**(原则 3,不拆):`capability`(+`builtin`)、`engine`(+6 引擎)、
`loop`、`store.KV`(低层原语,inmemory 留 L1)、`secrets`/`suspend`(非注册表)
——它们不是第三方后端,拆了反而散。这也顺带取消了上一版"拆 engine"的动作。

## 3. 目标结构

```
L1 协议层(功能名,零实现——除 store/engine 这类低层内聚件)
  capability/   source/   session/   memory/   channel/   prompt/   runctx/
  store/        ← 内聚:store.KV 低层原语协议 + inmemory 默认(todo/digest 的后端)
  engine/       ← 内聚:协议 + 6 个内置引擎(react/direct/… 不外移)
  secrets/  suspend/  ← 内聚:非注册表(硬编码 switch / 直接构造)
  model/        ← registry/ 改名(表功能;含 ModelFactory/RegisterModel/BuildModel)
  exec/         ← 新:从 exectool 上浮的脚本执行 Engine 协议(B 修复)
  vectorstore/  ← 新:从 vector 上浮的向量库后端协议(B 修复;避免撞 eino retriever)
  loop/ + builtin/  ← 核心机制/内置能力,内聚保留

L2 实现层  impl/<模块>/<实现>(按协议模块分组;每个实现一子包,init 自注册)
  impl/source/     http/ mcp/ a2a/ rpc/ local/ vector/ exec     (← provider/*)
  impl/session/    inmemory/ file/ redis/ bigram                (← session/*, provider/redisstore)
  impl/memory/     inmemory/ redis                              (← memory/*, provider/redisstore)
  impl/channel/    feishu                                       (← channel/feishu)
  impl/prompt/     inline/ file/ http                           (← prompt/providers.go)
  impl/model/      openai/ minimax                              (← provider/models)
  impl/vectorstore/ inmemory(词法保底)                          (← provider/vector 的 backend 部分)
  impl/interactor/ cli                                          (← interact/)
  impl/utils/      redisconn(redis 后端共享 dial)、其它共享件

  std/             聚合包:空导入即拉起全部 L2 默认,保 zero-config

  说明:
  · store.KV 内聚 L1(低层原语,todo/digest 用)——inmemory 默认随 store/ 常驻,
    永远可用;它的 redis 后端由 impl/session/redis 顺带注册 store "redis"(共用 dial)。
    无 impl/store、无 impl/redis。
  · session/memory 是真·可换后端模块:impl/session/{inmemory,file,redis,bigram}、
    impl/memory/{inmemory,redis},各注册一个协议、共享 impl/utils/redisconn。
  · impl/utils/redisconn = redis 后端共享连接件(Dial),普通包(不用 internal)。

L3 第三方(树外)
  开发者包:实现 L1 接口 + init Register,main 里空导入
  examples/engines/   ← 树内范例(docker exec 引擎,实现 exec.Engine)
```

**命名对照**(原则 1+2,泛名 → `impl/<模块>/<实现>`):
`provider/httptool` → `impl/source/http`;`provider/mcptool` → `impl/source/mcp`;
`provider/exectool` → `impl/source/exec`(+ 协议 `exec/`);`provider/vector` →
`impl/source/vector`(+ 协议 `vectorstore/`,词法后端 `impl/vectorstore/inmemory`);
`provider/redisstore` → `impl/session/redis`(兼注册 store "redis") + `impl/memory/redis`;
`provider/models` → `impl/model/openai` + `impl/model/minimax`;
`channel/feishu` → `impl/channel/feishu`;`interact` → `impl/interactor/cli`;
协议包内 inmemory/file/bigram 默认 → `impl/session/{inmemory,file,bigram}`、
`impl/memory/inmemory`、`impl/prompt/{inline,file,http}`。

## 4. 保 zero-config:`std` 聚合 + fail-fast

默认移出协议包后,协议包不能自引默认(否则 L1→L2 成环)。惯例做法(对齐
`database/sql` 无默认驱动、`image/png` 空导入):

- 每个默认在自己 `init()` 注册**类型名 + 空串默认**(如 `impl/session/inmemory` 注册
  `"inmemory"` 与 `""`)。
- `New("")` 查注册表 `""`;查不到 → 清晰报错(`no store backend: 空导入一个后端或 agent-kit/std`)。
- `std/` 聚合空导入全部 L2 默认;`examples/main.go` 与 config 测试改
  `import _ ".../std"`,一行恢复开箱即用。

## 5. 迁移分期(概览;逐文件步骤见 [layering-execution.md](layering-execution.md))

- **Tier 1 · B 修复(协议上浮)**:抽 `exec/`、`vectorstore/` 两个协议出实现层。面小独立。
- **Tier 2 · 单协议归位**:`provider/*`(source 族)、`channel/feishu`、`interact` →
  `impl/<模块>/<实现>`;包名基本不变(纯归位),`models` 拆 2、`interact` 改包名 cli。
- **Tier 3 · 默认/多协议实现外移**:session/memory 协议包内 inmemory/file/bigram +
  prompt 默认搬进 `impl/<模块>/<实现>`;`redisstore` 拆 `impl/session/redis`(兼注册
  store "redis")+ `impl/memory/redis` + `impl/utils/redisconn`;建 `std/`、`New("")` fail-fast。
  **store.KV 不动**(inmemory 留 L1)。
- **Tier 4 · 收尾(可选)**:`registry/` → `model/`。

**不动**(原则 3 内聚 / 非注册表):`engine`(+6 引擎)、`capability`、`loop`、
`builtin`、`store`(store.KV+inmemory)、`secrets`、`suspend`。

## 6. 收益

- 读协议包只见契约;想自实现的人一眼看到接缝,想替换 agent-kit 实现的人知道
  在换哪一份。
- `impl/` 一处收口全部 agent-kit 实现,第三方照抄同一模式(实现 L1 + init + 空导入)。
- 包名全表功能,无 `provider` 泛名;两个"藏在实现层的协议"(exec/retriever)浮出。
- 分层成可校验硬约束(lint:协议包禁 import `impl/*`)。

## 7. 决策(已按 impl/模块/实现 定)

- **结构**:`impl/<模块>/<实现>`,每个实现一子包;共享件进 `impl/utils/`(普通包,
  非 internal)。**不设 impl/redis / impl/store 聚合**。
- **store.KV 内聚 L1**:低层原语,协议 + inmemory 默认都留 `store/`(像 engine);
  其 redis 后端由 `impl/session/redis` 顺带注册 store "redis",无 impl/store。
- **redis**:`impl/session/redis`(注册 session + store)+ `impl/memory/redis`,
  共享 `impl/utils/redisconn`;按需精确空导入。
- **model**:`impl/model/openai` + `impl/model/minimax` 分开,各自空导入。
- **secrets/suspend**:**内聚保留**——非注册表(硬编码 switch / 直接构造),
  做成第三方接缝是独立 feature,不进本次搬迁。

待定:
1. **`registry` 改名 `model`**(泛名收尾)这轮做还是后置?(建议后置。)
2. **协议包命名**:脚本执行引擎 `exec/`(建议);向量后端 `vectorstore/`(避免撞 eino `retriever`)。
3. **`impl/prompt` 三源**:拆 `inline/file/http` 三子包(对称模块规则),还是并一包?
   (建议拆,统一规则。)

## 8. 下一步

先落 **Tier 1(B 修复)**:抽 `exec/`、`vectorstore/` 两个协议包,面小、独立、
编译器兜底,能先把"协议上浮"的形态坐实;A 修复(Tier 2,建 `impl/` + `std`)是
大范围平移,确认决策 1–3 后成批做。
