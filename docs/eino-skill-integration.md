# eino ADK Skill 与 agent-kit skillpack 的集成方案

> 状态:**方案评审中**。结论先行:**不建议用 eino 的 Skill middleware 替换自研
> skillpack 执行层**(它绑定 ADK 运行时,且不覆盖我们的治理/供应链/沙箱强制);
> 建议做**协议与接口双向对齐**——SKILL.md frontmatter 全兼容 + 提供 eino
> `skill.Backend` 适配器,技能资产两边通用,执行层各走各的。

## 1. 背景

agent-kit 已有完整技能栈:内部 YAML skill(steps/graph 编排)与外部 SKILL.md
技能包(skillpack)统一为 `cap://skill/*` 能力。eino v0.8+ 推出 ADK
ChatModelAgent Middleware 体系,其中 `adk/middlewares/skill` 提供了官方的
SKILL.md 技能支持(v0.9.12——我们当前依赖版本——已包含)。问题:要不要
"不重复造轮子",直接用 eino 的?

## 2. eino Skill middleware 能力盘点(源:官方文档 + v0.9.12 源码)

- **协议**:`FrontMatter{Name, Description, Context, Agent, Model}`;
  `Context` 取 `fork`(隔离上下文)/`fork_with_context`(复制历史)/空(内联)。
  与 anthropics/skills 的 SKILL.md 同源(agentskills.io)。
- **渐进式展示**:注入单个 `skill` 工具,描述里列全部技能的 name+description
  (L1);模型调用 `skill(name)` 时才读入完整 SKILL.md(L2);目录内其他
  文件按需读(L3)。
- **执行形态**:
  - 内联(默认):SKILL.md 正文作为工具结果返回,**当前 agent 继续执行**;
  - fork / fork_with_context:经 `AgentHub`/`ModelHub` 起**子 agent** 执行,
    可指定 agent 与模型(frontmatter 的 `agent:`/`model:` 字段)。
- **存储解耦**:`Backend interface { List(ctx) ([]FrontMatter, error);
  Get(ctx, name) (Skill, error) }` + `NewBackendFromFilesystem`(扫一级子目录)。
- **脚本执行不在 Skill middleware 里**:靠 Filesystem middleware 注入
  ls/read_file/write_file/edit_file/glob/grep + 可选 `execute` 工具;隔离取决
  于 `filesystem.Backend` 选型——local(宿主直跑)或火山引擎 Agentkit 云沙箱
  (AK/SK 付费服务,eino-ext)。
- **运行时绑定**:产物是 `adk.ChatModelAgentMiddleware`,只能挂进
  `adk.NewChatModelAgent(...Handlers)`;AgentHub 返回的是 `adk.TypedAgent`。
  **不能插入 compose react agent。**

## 3. agent-kit skillpack 现状(对照面)

| 维度 | eino Skill middleware | agent-kit skillpack |
|---|---|---|
| SKILL.md 解析/渐进展示 | ✅ | ✅(LoadManifest;L1 目录/L2 子循环 persona/L3 pack_read) |
| fork 子上下文 | ✅ AgentHub 起子 agent | ✅ BuildPack 子循环 + `context: fork` 快照 |
| frontmatter `agent:`/`model:` | ✅ | ❌(未实现,见 §5 P0) |
| 内联模式 | ✅(默认) | ❌(一律子循环) |
| **远程获取与供应链** | ❌ 只扫本地目录 | ✅ use: github@sha / https+integrity / file:;skills.lock 树哈希漂移 fail fast |
| **风险分级/目录准入** | ❌ | ✅ Risk=Dangerous + catalog max_risk |
| **审批/预算/超时/消化/断路** | ❌(需自配 ADK 其他 middleware) | ✅ applyGates 全套 Ring 0 下沉技能子循环 |
| **脚本沙箱强制** | ⚠️ 取决 filesystem backend:本地=裸跑,云=火山付费服务 | ✅ exec.Sandbox 四级解析 + require_sandbox(架构禁裸跑),docker 官方实现 |
| 文件读取囚笼 | ⚠️ backend 自行负责 | ✅ pack_read 路径囚笼 |
| 与内部技能统一 | ❌(独立工具) | ✅ 同 kind=skill 进 cap:// 目录,统一选品/审批/编排引用 |
| 运行时 | ADK ChatModelAgent | compose react + Ring 0 模型包装 |

## 4. 三个方案

### 方案 A:整体迁移 ADK,skill 用官方 middleware

主循环换 `adk.ChatModelAgent` + Runner + AgentEvent。
**代价**:runtime/engine、loop 的全部 Ring 0 包装(FinishGuard/Budget/Retry/
RepeatBreak/CheckedFinish)、PromptLayers、session/todo/digest/approval 接线
全部重做——它们挂在 `ToolCallingChatModel` 与 compose 工具链上;ADK 有自己
的 middleware 点位,等价物要重造。**得到**:官方演进红利(Summarization/
PlanTask/ToolSearch/Filesystem 全家桶)。
**判定:不做。** 迁移面 ≈ 重写运行时层,而 Skill middleware 本身不带治理、
不带供应链、不带沙箱强制——省掉的只是几百行解析与 fork 编排,换来运行时
锁定与治理真空。ADK 作为长期观察项(见 §6)。

### 方案 B:双运行时混跑

主循环留 compose react,技能执行单独起 ADK agent。
**判定:不做。** 两套会话/审批/预算/观测语义,Ring 0 无法穿透 ADK 侧,
复杂度最高。

### 方案 C(推荐):协议与接口双向对齐,执行层保留自研

技能**资产**(SKILL.md 目录)做到两边通用;**执行**留在 agent-kit 的
BuildPack + applyGates(治理不降级)。三件事:

1. **frontmatter 全兼容(P0)**:补 `agent:` 与 `model:` 字段——fork 时用
   指定的已装配 agent / 模型执行(我们已有 `context:` 对齐);未知字段容忍。
   收益:anthropics/skills、agentskills.io 生态与 eino 示例技能零改动可用。
2. **eino `skill.Backend` 适配器(P1,几十行)**:
   `einoskill.NewBackend(packRoot)` 把 EnsurePack 物化的
   `<work_dir>/agent-kit/.skills` 目录实现为 eino 的 `Backend` 接口
   (List/Get 直接映射 LoadManifest)。收益:用 eino ADK 的团队可直接消费
   我们分发/锁定/校验过的技能目录——供应链能力(@sha pin + skills.lock)
   变成对 eino 生态的增值,而非壁垒。反向亦然:eino 用户的本地技能目录
   `file:` 一行就能进 agent-kit。
3. **内联模式评估(P2)**:eino 默认内联(正文作为工具结果回给当前 agent)。
   我们一律子循环——上下文更干净但多一跳模型调用。对纯指令型技能
   (无脚本、短正文)可增加 `context: inline` 选项,BuildPack 退化为
   "返回 L2 正文 + 指路 L3"。收益:省一次子循环;风险:正文污染主上下文,
   需 catalog 准入照常把关。

## 5. 落地清单(方案 C)

- [ ] P0 `skill/pack.go` LoadManifest:解析 `agent:`/`model:`;BuildPack fork
      路径接 AgentHub 语义的本地等价物(按名查已装配 agent/模型,查不到
      fail fast)。冒烟:frontmatter 带 model 的技能路由到指定模型。
- [ ] P1 新包 `impl/skillbridge/einoskill`(impl 层,核心零依赖不变):
      实现 eino `skill.Backend`;单测:eino 侧 List/Get 与我们 LoadManifest
      结果一致。
- [ ] P2 `context: inline` 设计评审后实施。
- [ ] 文档:skillpack 文档声明"SKILL.md frontmatter 与 eino ADK Skill
      middleware 全兼容"。

## 6. 持续对照项(不是本方案范围,记录避免遗忘)

eino ADK middleware 与我们自研件同题:Summarization↔loop.Compactor
(它的 `Finalizer.PreserveSkills`——压缩后保留已加载技能内容,默认
5 个/单个 5000 token——直接命中我们"压缩摧毁技能上下文"的历史病灶,
值得抄进 Compactor)、PlanTask↔todo、ToolSearch/ToolReduction↔catalog
选品与 digest、Filesystem+Agentkit 沙箱↔exec.Sandbox、HITL/interrupt↔
suspend + approval。每次升级 eino 版本时对照其演进,能薅接口的薅接口
(如 compose 的 CheckPointStore 与我们 suspend 的 store.KV 天然同构),
不整体换血。

**架构级观察项**:官方 graph_or_agent 文档明确主推 ADK 承载开放型 agent,
compose 系 react(我们的主循环)被定位为"封闭任务用编排"的过渡形态;
v0.9 里 flow/react 仍正常维护,但长期演进红利会集中在 ADK 侧。触发重估的
信号:flow/react 停止跟进新能力,或我们需要 TurnLoop 级抢占/寻址式 HITL
时,再启动运行时迁移评估——届时本方案 C 已把技能资产做成两边通用,
迁移不受技能层牵制。
