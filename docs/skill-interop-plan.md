# Skill 协议兼容 + 脚本执行(exectool)方案

> 状态:**设计**,未落地。本文收口两件事:(1) 兼容市面上的 skill/工具协议
> 的整体策略;(2) 支撑其中"带脚本的技能"的执行原语 `exectool`(含自定义
> 沙箱引擎)。exectool 是第一优先落地项,SKILL.md 原生支持在其之上。

## 0. 动机

想让 agent-kit 能吃市面上的技能生态(Claude / OpenAI / Gemini / Trae),
且成本可控。核心结论先给:

- **没有统一的跨厂商 "skill 协议"**;但这不是该担心的轴。
- **skill 是模型无关的供给侧概念**,厂商差异落在**模型 API(tool-call 线
  格式)**上,而那一层框架已抽象(`registry.BuildModel` → eino
  `ToolCallingChatModel`)。
- 于是"支持各家 skill"**塌缩成 ~3 个模型无关格式适配器**,不是 N×M。

---

## 一、协议全景与策略

### 1.1 两个轴必须分开

| 轴 | 是什么 | 厂商差异 | 落哪 |
|---|---|---|---|
| **模型 API** | 工具 schema 怎么发、tool_call 怎么解析 | 有差异 | `registry` → eino,**已抽象** |
| **skill/工具供给** | 能力怎么被描述/发现/执行 | 小,且模型无关 | `source` → `capability` → 目录 |

一个 MCP server / SKILL.md 文件夹**不在乎哪个模型调它**;能力进目录后,任何
模型后端都能选它(`agentModel` 解析模型,能力面是另一条线)。所以"支持某家
skill" ≠ "支持某家模型"。

### 1.2 市面协议其实就三四家

| 协议 | 是什么 | agent-kit 现状 |
|---|---|---|
| **MCP** | 工具/资源/提示词,JSON-RPC,固定 schema | ✅ 已原生(`provider/mcptool`) |
| **A2A** | agent 间调用 | ✅ 已原生(`provider/a2a`) |
| **Anthropic Agent Skills**(`SKILL.md`) | 指令 + 打包脚本,模型自主、渐进披露 | ❌ **缺口** |
| OpenAPI Actions / GPTs 插件 | REST + schema | 半覆盖(`http` source 兜底),低优先 |

**收敛点是 MCP**:各家(含 Trae 等 IDE)都在往支持 MCP 靠,它是最接近跨厂商
标准的东西,已覆盖。所以"兼容市面协议"落到实处 = **补 SKILL.md** + 一个能跑
脚本的执行原语(exectool)。

### 1.3 策略

- **不按"厂商 × 协议"规划**(会得出假的 N×M 大工程),按**格式**规划:
  MCP(有)、A2A(有)、SKILL.md(建)、OpenAPI(http 兜底,可选加 importer)。
- 想"支持三家"最实在的一步其实是**补齐 model provider**(让 agent 跑在
  GPT/Gemini 上),而非适配它们的"skill"——skill 侧大家都在往 MCP 收敛。
- 两个真实成本(都不是"协议数量"):弱模型上 SKILL.md 类技能表现差(模型
  能力问题)、per-model tool-call 怪癖(eino 抹平大部分,边角在 provider 打补丁)。

---

## 二、概念澄清:agent-kit 的 `skill` ≠ Anthropic 的 `Skill`

**这是最要命的一点,先讲清,否则会错误地把 SKILL.md 塞进现有 `skill`。**

- **agent-kit `skill`** = **确定性编排图**(steps/params 固定,运行时无大脑,
  路径强保证)。
- **Anthropic `Skill`** = 一包**指令 + 可选脚本**,**模型读了自己发挥**
  (渐进披露:先 name/description,判定相关再加载正文,再按需读脚本/资源)。

两者哲学相反。SKILL.md 天然对应的不是 `skill`,而是:

> **`component`(engine: react)+ 懒加载的 prompt + 一个受控 exec/fs 工具面**

| SKILL.md | agent-kit 原语 | 渐进披露 |
|---|---|---|
| frontmatter `name`/`description` | `capability.Meta`(进目录,供选品) | **L1**(始终在上下文) |
| Markdown 指令正文 | 选中后注入的 react 系统 prompt | **L2**(路由到才加载) |
| 打包脚本/资源 | bundle 目录 + 受控 exec/fs 工具(见第四节) | **L3**(按需读/执行) |
| `allowed-tools` | 工具面白名单(已有机制) | 权限锁定 |

**命名**:新形态叫 `agentskill` / `skillpack`,**别撞现有 `skill`**。它是与
`skill`(确定性图)、`component`(引擎单元)、MCP tool(固定 schema)并列的
**第四种能力形态**:模型自主、指令驱动、带打包代码。

---

## 三、脚本执行原语:`exectool`(先落地)

SKILL.md 的"L3 执行脚本"需要一个受控执行底座;它本身也独立有用(给任意
agent 一个跑脚本的工具)。**形态镜像 httptool**:一个 `exec` source,config
里列几个工具、各绑一种运行时,`Sync` 吐出对应的 cap。

### 3.1 CapRef:用 `kind=tool`,不新开 `exec` kind

- 按协议,`kind` 是**结构类别**(tool/skill/component/agent),不是风险类别;
  "能跑代码"是 `Risk=Dangerous` 的事——协议连 `model` 都没给 kind,就是这个
  原则。
- `kind=tool` 直接吃现有管线:namespace `tools/exec/python` 可解析
  (`tools/<源>/<name>` → `cap://tool/<源>/<name>`,domain 恒=源名),
  `capabilities.include: ["cap://tool/exec/*"]` 一把选中,治理分组照样有。
- 生成 ref:`cap://tool/<源名>/<工具名>`,如源名 `exec` → `cap://tool/exec/python`。

> 约束:`kind=tool` 时 domain 恒等于**源名**(ns 的 `tools/` 解析靠它),故
> 语言落在 **name 段**(`.../python`)。若坚持语言落 domain(`cap://exec/python/x`)
> 需给 callable 家族**加第 8 个 kind**,动 catalog 选品/跨 ns 引用/`tools/` 解析
> ——**不推荐**,`Risk=Dangerous` + `cap://tool/exec/*` 已够治理。

### 3.2 每个 cap:入参只有 script + args

工具固定一种脚本类型(python exec tool 只跑 python),所以连 `type` 都不用:

```go
capability.New(capability.Meta{
    Ref:         capability.Ref{Kind: "tool", Domain: srcName, Name: t.Name}, // cap://tool/exec/python
    Description: "用 " + t.Runtime + " 执行脚本并返回输出",
    Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
        "script": {Type: schema.String, Desc: "脚本内容", Required: true},
        "args":   {Type: schema.String, Desc: "参数,空格分隔(可空)"},
    }),
    Risk: capability.RiskDangerous,   // 默认不入目录,须 catalog.max_risk: dangerous
}, run)
```

### 3.3 三级执行解析(cap 内部)

```
tool.engine 指定   → 用注册的 Engine.Exec(script, args)        # 自定义沙箱/远程/WASM
否则 tool.command  → exec.CommandContext(command..., script, args...)  # 命令里包一层沙箱
否则               → 内置模板(python3 -c / node -e / bash -c / sh -c),宿主直跑
```

内置模板:`python:[python3 -c]`、`node:[node -e]`、`bash:[bash -c]`、
`sh:[sh -c]`。**bash/sh 加 `$0` 占位**(args 从 `$1` 起),python/node 不加
(args 直接进 `sys.argv[1:]` / `process.argv[2:]`)。非零退出/超时**作结果
回传**(附 exit+输出),不返 Go error——让大脑看到报错自己改,不中断循环。

### 3.4 沙箱引擎可定制(核心:框架不拥有隔离)

框架出**接缝**,隔离实现是使用方的。两级:

**Level 1 — config 覆盖命令(零代码)**:把沙箱包进命令。
```yaml
- {name: python, runtime: python,
   command: ["docker","run","--rm","-i","--network=none","--memory=512m","py-sandbox","python3","-c"]}
```

**Level 2 — 注册 Go 引擎**(命令模板表达不了时:远程服务/WASM/常驻池):
```go
// exectool 暴露的注册点
type Engine interface {
    Exec(ctx context.Context, script string, args []string) (string, error)
}
type EngineFactory func(conf map[string]any) (Engine, error)
func RegisterEngine(name string, f EngineFactory)
```

**默认(内置模板)= 宿主直跑,零隔离**——开发用;生产必须 Level 1 或 Level 2。
`agent-kit 不拥有任何沙箱实现`。

### 3.5 完整配置示例(自定义 docker 引擎跑 python)

**使用方的引擎**(空导入即注册):
```go
package engines
func init() {
    exectool.RegisterEngine("docker", func(conf map[string]any) (exectool.Engine, error) {
        img, _ := conf["image"].(string)
        if img == "" { return nil, fmt.Errorf("docker engine: image required") }
        // ...读 network/memory/timeout...
        return &dockerPy{image: img /*, ...*/}, nil
    })
}
type dockerPy struct{ image, network, memory string; timeout time.Duration }
func (e *dockerPy) Exec(ctx context.Context, script string, args []string) (string, error) {
    argv := append([]string{
        "run","--rm","-i","--network="+e.network,"--memory="+e.memory,"--cpus=1",
        "--pids-limit=128","--read-only","--tmpfs=/tmp:size=64m",
        "--cap-drop=ALL","--security-opt=no-new-privileges",
        "-w","/tmp", e.image, "python3","-c", script,
    }, args...)
    out, err := exec.CommandContext(ctx, "docker", argv...).CombinedOutput()
    if err != nil { return fmt.Sprintf("exit error: %v\n%s", err, out), nil }
    return string(out), nil
}
```

**main.go**:
```go
import (
    _ "github.com/joewm9911/agent-kit/provider/exectool" // type: exec 这个 source
    _ "yourapp/engines"                                   // "docker" 引擎
)
```

**app.yaml**(放行 dangerous 准入):
```yaml
catalog: {max_risk: dangerous}   # 否则 exec 工具被目录挡在门外
model:   {provider: minimax, config: {api_key: "${MINIMAX_API_KEY}"}}
agents:  [agents/coder.yaml]
```

**namespaces/scripting.yaml**(4 工具,python 走 docker 引擎,其余各异):
```yaml
tools:
  - name: exec                 # 源名 → cap 的 domain
    type: exec
    config:
      timeout: 30s             # 模板执行的墙钟兜底
      tools:
        - name: python         # → cap://tool/exec/python
          runtime: python
          engine: docker
          engine_config: {image: python:3.12-slim, network: none, memory: 512m, timeout: 30s}
        - name: node           # → cap://tool/exec/node(内置模板,宿主)
          runtime: node
        - name: bash           # → cap://tool/exec/bash(Level 1 包 firejail)
          runtime: bash
          command: ["firejail","--quiet","bash","-c"]
        - name: sh
          runtime: sh
components:
  - name: analyst
    engine: react
    prompt: "你是数据分析师。需要计算/处理数据时用 python 执行脚本。问题:{q}"
    tools: ["tools/exec/python"]
skills:
  - name: analyze
    description: 用脚本分析数据并给结论
    params: {q: {type: string, required: true}}
    use: "components/analyst"
```

**agents/coder.yaml**:
```yaml
description: 会写并执行脚本的助手
loop: {max_steps: 20}
namespaces: [../namespaces/scripting.yaml]
capabilities: {include: ["cap://tool/exec/*"]}   # 主循环也拿到四个工具
approval:     {mode: interactive}                # dangerous exec 每次走人工批准
```

生成:`cap://tool/exec/{python,node,bash,sh}`,分别走 docker / 宿主模板 /
firejail / 宿主模板。运行时:cap → Ring 0(审批/超时/轨迹)→ 解析出的引擎 →
输出回传。

### 3.6 免费得到的安全(在 scope 内的部分)

- `Risk=Dangerous` → 默认不入目录,须 `catalog.max_risk: dangerous` 放行;默认关。
- 照过 Ring 0:`GateApproval`(危险 exec 人工批准)、`TimeoutTools`(墙钟)、
  `RecordTools`(轨迹)、`DurableEffects`(挂起重放不二跑)。
- **进程/资源/网络隔离 = 部署负责**(容器/VM 里跑 agent-kit,或 Level 1/2 引擎)。

---

## 四、SKILL.md 原生支持(`agentskill` source,在 exectool 之上)

一个 `agentskill` source,扫描 skill 目录(`~/.claude/skills/`、项目
`.claude/skills/`、或打包进镜像的 `/opt/skills`),把 SKILL.md 变成能力:

1. `Sync` 读每个 `SKILL.md` frontmatter → `capability.Meta`(description = 那句
   "何时用",进目录供选品)= **L1**。
2. 能力被调用 = 拉起一个 react component:system prompt = SKILL.md 正文
   (**L2**),工具面 = **exectool 的 exec 工具**(限定 bundle 目录)+ 只读 fs
   工具(读资源,**L3**),按 `allowed-tools` 白名单。
3. 风险默认 `dangerous`,安装期校验 artifact `sha256`/签名(纵深防御,沙箱不是
   唯一防线)。

映射见第二节表。**执行底座就是第三节的 exectool**——这就是为什么 exectool 先做。

---

## 五、整体装配:三者怎么串

```
MCP(mcptool)      ── 工具/资源协议 ── 已有
A2A(a2a)          ── agent 间协议  ── 已有
exectool           ── 受控脚本执行原语(cap://tool/exec/*,引擎可插拔)── 建①
agentskill         ── SKILL.md → react component,用 exectool 跑 L3 脚本 ── 建②
OpenAPI/Actions    ── http source 兜底,必要时加 importer ── 可选
```

模型无感知这些差异:统统进目录、被选品、挂工具面。厂商差异在模型 API 层被
eino 抹平,skill 供给层保持模型无关。

---

## 六、实施顺序与工作量

| 阶段 | 内容 | 工作量 | 备注 |
|---|---|---|---|
| **P1** | `provider/exectool`:source + cap + 三级解析 + 内置四模板 + `RegisterEngine` | 小 | 离线可测(模板一条 + 假 engine 一条 + 非零退出) |
| **P1.5** | docker 引擎 example 放 `examples/engines/` | 小 | 需 docker 环境才跑,非必测 |
| **P2** | `agentskill` 只读子集:SKILL.md → react component,只给只读 fs(不执行脚本) | 中 | 零代码执行 = 零沙箱风险,覆盖纯指令型技能 |
| **P3** | `agentskill` + exec:接 exectool,跑 bundle 脚本 | 中 | 依赖 P1 |
| **P4** | `agent-kit skills sync` 子命令:构建镜像时下载+校验+烘入 | 中 | 复用于 SKILL.md / MCP server 打包 |
| 可选 | OpenAPI importer(Actions/Extensions) | 中 | http source 已兜大半,按需 |

---

## 七、决策记录

**已锁**
- exectool 用 `kind=tool`,**不新开 `exec` kind**(kind 是结构类别,非风险类别)。
- 每 runtime 一个 cap(`run` 不合一);入参仅 `script` + `args`。
- 三级执行解析:`engine` > `command` > 内置模板。
- 沙箱引擎可插拔(`RegisterEngine`);**框架不含任何沙箱实现**,隔离交部署。
- exec 默认 `Risk=Dangerous`,须 `catalog.max_risk: dangerous` 放行。
- SKILL.md 落到新形态 `agentskill`,**不复用/不污染现有 `skill`**。

**待定**
- `exectool` 默认内置哪些运行时模板(建议 bash/sh/python/node,**默认全不启用**,
  config 显式列出才生效)。
- `agentskill` 的 skill 目录发现规则(固定路径 vs 配置)。
- `skills sync` 的 artifact 来源优先级(OCI / git / tarball URL)。
- macOS 无强隔离:降级弱隔离(告警)还是"要求 Docker,否则拒装 exec 技能"。

---

## 八、下一步

先落 **P1 `provider/exectool`**(纯 seam,离线可测,不含任何真隔离),让形状能
跑起来看;docker 引擎作为 example;SKILL.md(P2/P3)在其之上分期推进。
