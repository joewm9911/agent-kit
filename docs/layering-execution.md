# 三层分离 · 改造执行文档

> 配套 [layering-plan.md](layering-plan.md)。本文是**逐步可执行清单**:每步给
> 文件移动、import 重写、验证命令、回滚点。全程**纯归属搬迁 + 改名,零行为
> 变更**,靠编译器 + `-race` 兜底。
>
> **前置**:独立分支 `git switch -c layering`,按 Tier 顺序做,每 Tier 结束跑
> §0 验证 + 提交,可随时回滚到上一 Tier。

## 0. 通用验证(每 Tier 结束都跑)

```bash
gofmt -l .                       # 无输出 = 格式干净
go build ./...                   # 必须过
go vet ./...                     # 必须过
go test -race ./...              # 全绿(redis 测试无 redis 会 skip,正常)
```

**分层守卫**(L1 协议包禁 import `impl/`):
```bash
for p in capability source store session memory engine channel prompt \
         secrets suspend runctx registry exec retriever; do
  grep -rq "agent-kit/impl/" "$p"/ 2>/dev/null && echo "❌ VIOLATION: $p imports impl/"
done; echo "guard done"
```

**回滚**:`git reset --hard HEAD`(未提交)或 `git revert <tier-commit>`。

## 0.1 影响面(为什么成本低)

真正 import 这些实现包的地方(非注册解耦):
- `examples/main.go` — 空导入全部 provider + `interact.NewCLI()` + `local.Func`。
- `config/{stress_live_test,store_e2e_test,example_yaml_test,smoke_e2e_test}.go` — 空导入。

其余一律靠 string 注册表 + 空导入,**改 import 路径即可,包名/选择器不动**。

## 0.2 本次不动的(原则 3 内聚 / 非注册表)

- `engine/`(6 个内置引擎)、`capability/`、`loop/`、`builtin/` — 框架机制/核心抽象,内聚保留。
- **`store/`(store.KV + inmemory)— 低层原语,inmemory 默认留 L1、常驻可用**;不设 impl/store。
- `secrets/`(Env/File)、`suspend/`(FileStore)— **硬编码 switch / 直接构造,不是注册表**;
  它们要成为第三方接缝是独立 feature,本次保持原样。

---

## Tier 1 — B 修复:协议上浮(`exec/`、`vectorstore/`)

**目的**:把长在实现层的两个协议定义提到 L1。面小、独立。

### 1.1 新建 `exec/`(脚本执行引擎协议)
从 [provider/exectool/exectool.go](../provider/exectool/exectool.go) 剪切**协议部分**到新文件 `exec/exec.go`:
- `type Engine interface`、`type EngineFactory`、`RegisterEngine`、`engineFactory`、
  `engMu`/`engines` 变量。
- `package exec`;import `context`。

`provider/exectool` 保留 source 实现,改为 `import "…/exec"`,把内部 `Engine`/
`engineFactory(...)` 引用改成 `exec.Engine`/`exec.Lookup(...)`(顺手把
`engineFactory` 导出为 `exec.Lookup` 供 source 查引擎)。

### 1.2 新建 `vectorstore/`(向量库后端协议)
从 [provider/vector/vector.go](../provider/vector/vector.go) 剪切协议部分到 `vectorstore/vectorstore.go`:
- `type BackendFactory`、`RegisterBackend`、`newBackend`(改名 `New`)、注册表变量。
- `package vectorstore`(**不叫 retriever**——会与 eino 的 `retriever` 包选择器冲突;
  它返回的是 eino `retriever.Retriever`,vectorstore 是"产出 retriever 的后端")。
- vector source 实现留 `provider/vector`,改 import `vectorstore`。

> 内置 `inmemory` 词法后端(vector.go 里 `RegisterBackend("inmemory", …)`)属实现,
> 暂留 `provider/vector`(Tier 2 随包搬 `impl/vector`)。

### 1.3 验证 + 提交
跑 §0。提交:`git commit -m "Extract exec/ and vectorstore/ protocols out of impl layer"`。

---

## Tier 2 — 单协议实现归位 `impl/<模块>/<实现>`

**目的**:消灭 `provider/` 泛名,把**单协议**实现按模块归位。**包名不变**
(叶子沿用现包名,避免撞 stdlib 如 `http`;`impl/channel/feishu` 里包仍叫
`feishu`),选择器引用零改;只有 `models`(2 拆)、`interact`(改名 cli)、
`vector`(拆出 backend)例外,下述。

### 2.1 source 族(单协议,纯归位)
```bash
mkdir -p impl/source
for pkg in httptool mcptool a2a rpctool local exectool; do
  git mv provider/$pkg impl/source/$pkg
done
```
包名不变(`httptool`/`mcptool`/…),import 路径重写:
```bash
grep -rl 'agent-kit/provider/' --include='*.go' . | \
  xargs sed -i '' -E 's#agent-kit/provider/(httptool|mcptool|a2a|rpctool|local|exectool)#agent-kit/impl/source/\1#g'
```

### 2.2 channel / interactor
```bash
mkdir -p impl/channel impl/interactor
git mv channel/feishu impl/channel/feishu           # 包名 feishu 不变
git mv interact impl/interactor/cli                 # 包名 interact → cli
```
重写:
```bash
grep -rl 'agent-kit/channel/feishu' --include='*.go' . | xargs sed -i '' 's#agent-kit/channel/feishu#agent-kit/impl/channel/feishu#g'
grep -rl 'agent-kit/interact"'      --include='*.go' . | xargs sed -i '' 's#agent-kit/interact"#agent-kit/impl/interactor/cli"#g'
```
把 `impl/interactor/cli` 的 `package interact` 改 `package cli`;`examples/main.go`
的 `interact.NewCLI()` → `cli.NewCLI()`(选择器随包名改,仅此一处)。

### 2.3 model 族(1 包拆 2)
```bash
mkdir -p impl/model/openai impl/model/minimax
git mv provider/models/openai.go  impl/model/openai/openai.go     # 改 package models→openai
git mv provider/models/minimax.go impl/model/minimax/minimax.go   # 改 package models→minimax
rmdir provider/models
grep -rl 'agent-kit/provider/models' --include='*.go' . | \
  xargs sed -i '' 's#agent-kit/provider/models#agent-kit/impl/model/openai#g'   # 空导入者需拆成两条,见下
```
> 空导入 `provider/models` 的地方(examples/main.go)拆成
> `_ ".../impl/model/openai"` + `_ ".../impl/model/minimax"`。

### 2.4 验证 + 提交
跑 §0 + 守卫。提交:`git commit -m "Regroup single-protocol impls under impl/<module>/<impl>"`。

> `provider/vector`、`provider/redisstore` 留到 Tier 3(它们要拆 backend / 拆多协议)。
> `examples/engines`(docker exec 引擎,L3)不动。

---

## Tier 3 — A 修复:默认/多协议实现移出协议包 + `std` + fail-fast

**目的**:协议包只剩契约。**最有价值也最需谨慎**的一步。全部按
`impl/<模块>/<实现>` 归位。

### 3.1 内存默认(进程内零依赖)——session/memory 按模块拆
各自新子包,`init` 自注册对应协议:
- `memory/` 的 `memStore`+`NewInMemory` → `impl/memory/inmemory`(注册 memory `"inmemory"`)。
- `session/` 的 inmemory 分支 → `impl/session/inmemory`(注册 session `"inmemory"`)。
- `session/recall.go` 的 `bigramRetriever` → `impl/session/bigram`(注册 retriever `"bigram"`)。
- `session/` 的 `NewFileStore` → `impl/session/file`(注册 session `"file"`)。

协议包删掉对应实现体与 `init` 注册;**保留** `New(typ)` 的 `""→"inmemory"` 重映射。

> **store.KV 不动**:`store/inmemory.go` + 其 `RegisterBackend("inmemory")` 留在 L1
> `store/`(低层原语,常驻可用)。无 `impl/store`。

### 3.2 redis 后端——session/memory 各一包 + 共享 dial
从 `provider/redisstore/redis.go` 拆:
- `dial` + config 解析 → `impl/utils/redisconn`(`package redisconn`,导出 `Dial(conf)`)。
- `sessStore` + init → `impl/session/redis`;**顺带把 `kv` 类型 + `store.RegisterBackend("redis")`
  一并放这**(store.KV 的 redis 后端寄居于此,共用 redisconn)。
- `memStore` + init → `impl/memory/redis`(注册 memory `"redis"`)。
- **不设 impl/store、impl/redis**:分布式 todo/digest+session 空导入 `impl/session/redis`,
  长期记忆再加 `impl/memory/redis`。
- `redis_test.go` 随之拆。
```bash
rm -rf provider/redisstore   # 内容已搬
```

### 3.3 vector:source 与 backend 分离
- source 部分 → `impl/source/vector`(Tier 2 若已挪,此处只拆 backend)。
- 内置词法 backend(`RegisterBackend("inmemory", …)`)→ `impl/vectorstore/inmemory`。

### 3.4 提示词源
[prompt/providers.go](../prompt/providers.go) 拆 `impl/prompt/{inline,file,http}`
(或并一包 `impl/prompt/builtin`,见 plan 决策 3)。`prompt/` 只留接口 + `Register` + `NewProvider`。

### 3.5 `New("")` fail-fast(session/memory 协议包)
`session.New`/`memory.New`/`session.NewRetriever` 查不到时(store.KV 因 inmemory 常驻
不受影响):
```go
fmt.Errorf("session: no %q store registered; blank-import e.g. agent-kit/impl/session/inmemory or agent-kit/std", typ)
```

### 3.6 新建 `std/`(zero-config 聚合)
```go
// Package std 空导入即拉起 agent-kit 默认实现,恢复开箱即用。
package std
import (
    // store.KV 的 inmemory 在 L1 store/ 常驻,无需在此空导入。
    _ "github.com/joewm9911/agent-kit/impl/session/inmemory"
    _ "github.com/joewm9911/agent-kit/impl/session/file"
    _ "github.com/joewm9911/agent-kit/impl/session/bigram"
    _ "github.com/joewm9911/agent-kit/impl/memory/inmemory"
    _ "github.com/joewm9911/agent-kit/impl/prompt/inline"
    _ "github.com/joewm9911/agent-kit/impl/prompt/file"
    _ "github.com/joewm9911/agent-kit/impl/prompt/http"
    _ "github.com/joewm9911/agent-kit/impl/vectorstore/inmemory"
)
```

### 3.7 修复消费点
- `examples/main.go`:加 `_ ".../std"`(生产再按需 `_ ".../impl/session/redis"` 等)。
- `config/*_test.go`:依赖默认后端的加 `_ ".../std"` 或精确子包;逐个 `go test`,
  fail-fast 报 "no X backend" 就补——**精确点名,不会静默错**。

### 3.8 验证 + 提交
跑 §0 + 守卫(此刻 `store`/`session`/`memory`/`prompt` 应零 impl import)。
提交:`git commit -m "Extract default & redis backends to impl/<module>/<impl>, add std, fail-fast"`。

---

## Tier 4 —(可选)收尾泛名

- `registry/` → `model/`:`git mv registry model` + 改包名 `registry`→`model` + 重写
  import;`DecodeConfig` 这类通用助手挪 `internal/config` 或留 `model`。**改动面中等**
  (registry 被 config、provider/models 直接引用),单独一 Tier。
- 复核所有包名是否"表功能"(§原则 1),个别再调。

---

## 一眼总览:移动表

| 从 | 到 | 类型 | Tier |
|---|---|---|---|
| `provider/exectool`(协议部分) | `exec/` | 协议上浮 | 1 |
| `provider/vector`(协议部分) | `vectorstore/` | 协议上浮 | 1 |
| `provider/{httptool,mcptool,a2a,rpctool,local,exectool}` | `impl/source/<同名>` | 归位 | 2 |
| `channel/feishu` | `impl/channel/feishu` | 归位 | 2 |
| `interact` | `impl/interactor/cli`(包名→cli) | 归位+改名 | 2 |
| `provider/models`(openai/minimax) | `impl/model/openai` + `impl/model/minimax` | 1拆2 | 2 |
| `store/inmemory.go` | **不动**(留 L1 `store/`) | 内聚 | — |
| `memory` inmemory | `impl/memory/inmemory` | 实现外移 | 3 |
| `session` inmemory / file / bigram | `impl/session/{inmemory,file,bigram}` | 实现外移 | 3 |
| `provider/redisstore` | `impl/session/redis`(兼 store "redis")+ `impl/memory/redis` + `impl/utils/redisconn` | 拆 | 3 |
| `provider/vector`(backend) | `impl/source/vector` + `impl/vectorstore/inmemory` | 拆 | 3 |
| `prompt/providers.go` | `impl/prompt/{inline,file,http}` | 实现外移 | 3 |
| (新) | `std/` 聚合、`impl/utils/` 共享 | — | 3 |
| `registry/` | `model/` | 泛名收尾 | 4(可选) |
| `store/` `secrets/` `suspend/` `engine/` `capability/` `loop/` `builtin/` | **不动** | 内聚保留 | — |

## 风险与顺序

- Tier 1/2 **低风险**(编译器全兜),先做,快速验证形态。
- Tier 3 **需逐个补空导入**,fail-fast 会精确指路,不会静默出错;是唯一"可能漏"的
  地方,靠 `go test -race ./...` 收口。
- 每 Tier 独立提交,坏了 `git revert` 单 Tier,不牵连。
- 全程零行为变更:同一份实现,只是换了包;运行时注册表内容不变(名字一样)。
