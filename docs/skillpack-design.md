# Skillpack:外部 skill 链接引用、.skills 本地化与框架内执行

> 状态:**已落地**(批 1-5 全部实现,live 冒烟 18_Skillpack 真实 MiniMax 验证;打包期 CLI 按决策延后)。前置文档
> [skill-interop-plan.md](skill-interop-plan.md) 定了概念与分期;本方案落成
> 可实施设计,需求收口为三句话:
> 1. 在现有 skill 配置里**直接写外部链接**(`use:`)即完成集成;
> 2. 外部 skill 在**镜像打包期或应用启动期**下载到工程本地
>    **`.skills/` 目录**(vendoring,运行期零网络);
> 3. 在 agent-kit 自己的引擎与 Ring 0 治理体系**内**执行,现有 skill
>    (确定性编排)零改动。

## 1. 总览:vendoring 模型

外部 skill 的生命周期对齐 Go modules 的心智(`go.sum` + `vendor/`)。
**v1 以启动期下载为主路径**(已拍板);镜像打包期物化(CLI sync)延后:

```
声明(app.yaml 里 use: <ref>)
   │
   └── 启动期:Build/BuildApp 装配(v1 主路径)
                ├─ .skills 命中且 lock 校验通过 → 直接装配(零网络)
                ├─ 缺失 → 下载 → 校验 → 写 .skills + skills.lock → 装配
                └─ 内容与 lock 不符(被篡改/漂移)→ fail fast

   (延后)打包期:agentkit skills sync → .skills 随镜像分发,
            启动期配 require-local。库函数先留口(§6.2),CLI 后置。

运行期(Run/Stream):永不出网、永不写 .skills——只读已物化的包。
```

要点:
- **`.skills/` 是工程一等公民**:目录可读、可提交 git、可 COPY 进镜像;
  不是藏在用户目录的缓存。
- **`skills.lock` 是供给链事实**:ref → 解析版本 + 内容 sha256;提交进
  git 后,外部技能的任何变更都在 code review 里可见(对齐 go.sum)。
- **一个真相源**:启动期补齐与(将来的)打包期 sync 走同一段库代码,
  产物字节一致——首次启动下载后,`.skills` + lock 已就位,重启即零网络;
  等打包期 CLI 落地后,生产切 `require-local` 即完成收紧,应用零改动。

## 2. 概念定位:第四种能力形态(沿用 interop 结论)

| 形态 | 本质 | 运行时 | cap kind |
|---|---|---|---|
| skill | 确定性编排图(steps 固定,无大脑) | graph/workflow | `skill` |
| component | 单引擎执行单元 | react/plan-execute/... | `component` |
| MCP tool | 远端固定 schema 工具 | 远端 | `tool` |
| **skillpack** | **指令包 + 可选脚本,模型自主发挥** | **本框架 react 循环** | **`skill`(已拍板:与内部一致)** |

kind 与内部 skill 统一(已拍板,推翻初版建议):对消费方"skill 就是
skill"——agent 的 include 选品、审批规则作者不必关心来源;溯源是属性,
在 `Tags`(`ref:<来源> sha:<内容哈希>`)与 lock 文件里;风险治理靠 Risk
分级(脚本包自动 Dangerous 过准入),不靠 kind。

SKILL.md 渐进披露 → agent-kit 落点:

| SKILL.md | 落点 | 披露层 |
|---|---|---|
| frontmatter `name`/`description` | `capability.Meta` 进目录,供大脑选品 | L1(常驻) |
| Markdown 指令正文 | 被调用时注入内部 react 循环的系统提示词 | L2(选中才加载) |
| 打包脚本/资源 | `pack_read` 只读工具 + exectool 执行 cap | L3(按需) |
| `allowed-tools` | 工具面白名单(与目录选品求交集) | 权限锁定 |

## 3. 链接格式(ref)与锁定

```
github.com/<owner>/<repo>[/<subdir>]@<tag|sha>     # 短写,经 codeload zip
https://<host>/<path>.(zip|tar.gz)                 # 直链归档
file:<path>                                        # 本地目录(开发/私有分发)
```

- **pin 默认强制**:未 pin(无 @tag/@sha 且非 file:)的 ref,sync 与启动
  一律 fail fast;显式 `allow_unpinned: true` 才放行(且 lock 会把解析出的
  实际 sha 锁死,下次漂移即报)。
- **完整性**:条目可声明 `integrity: sha256:<hex>` 强校验归档字节;未声明
  时首次 sync 计算并写入 lock,之后按 lock 校验。

## 4. `.skills/` 目录与 lock 文件

agent-kit 是 SDK:落盘产物收口在宿主项目工作目录的 `agent-kit/` 命名空间
下(对齐 node_modules / .terraform 心智)。`work_dir` 可配(app 级,默认
进程 cwd);`skillpacks.dir` 是完全覆盖的逃生口(相对值同以 work_dir 为基准)。

```
<PROJECT_WORK_DIR:work_dir 可配,默认进程 cwd>/
└── agent-kit/                         # SDK 产物命名空间(固定约定)
    └── .skills/
        ├── skills.lock                # 供给链事实,建议提交 git
        ├── docs/pdf@<sha>/            # <ns>/<name>@<version>/ 解包内容
        │   ├── SKILL.md
        │   └── scripts/…
        └── writing/report-writer@…/
```

`skills.lock`(YAML,机器写、人可读):

```yaml
packs:
  - ref: "github.com/anthropics/skills/pdf@v1.2.0"
    name: docs/pdf                # 解析后的 ns/name(本地覆盖后的最终名)
    version: v1.2.0
    sha256: "9f2a…"               # 解包后内容树哈希(逐文件哈希再聚合)
    synced_at: "2026-07-05T12:00:00Z"
```

规则:
- 目录路径 = 最终 `<ns>/<name>@<version>`(可读、可 diff);哈希在 lock,
  不用内容寻址目录名——vendoring 要的是评审友好。
- 启动期逐包校验:目录存在 + 内容树哈希 == lock;不符即 fail fast
  (信息指明哪个包、期望/实际哈希)。
- 位置由 work_dir 决定(见上);`skillpacks.dir` 可整体覆盖(多应用共享等场景)。

## 5. 配置面:同一列表,内外混排

```yaml
# app 级
work_dir: "."                   # 宿主项目工作目录(默认进程 cwd)
skillpacks:                     # 安装目录固定:<work_dir>/agent-kit/.skills
  sync: auto                    # auto(缺失即下载)| require-local(生产建议)
  allow_unpinned: false

skills:
  - name: research/competitor_report       # 内部 skill,原样
    steps: [...]

  - use: "github.com/anthropics/skills/pdf@v1.2.0"     # 外部,一行集成

  - use: "https://skills.example.com/report-writer.zip"
    integrity: "sha256:9f2a…"
    name: writing/report-writer   # 覆盖 ns/名(默认取 frontmatter)
    model: {provider: minimax, config: {...}}
    max_steps: 12
    tools:                        # allowed-tools 之上再收紧(交集)
      - "cap://tool/exec/python"
```

- `use` 与 `steps/engine/prompt` 互斥(fail fast)。
- schema 落点:config 层 `SkillEntry { skill.Declaration ",inline";
  Use, Integrity string; Tools []string }`,`skills:` 列表类型从
  `[]*skill.Declaration` 换成 `[]*SkillEntry`;namespace 的 skills 段同构。
  **skill 包零改动,存量 YAML 零迁移**(inline 兼容)。

## 6. 获取时机

### 6.1 启动期下载安装(v1 主路径)

`Build/BuildApp` 遇到 `use:` 条目:

```
resolveLocal(entry, lock, dir)
  ├─ 命中且哈希符 → parse SKILL.md → skill.BuildPack → catalog.Add
  ├─ 缺失:
  │    ├─ sync: auto(默认)   → fetch + 校验 + 写 .skills/lock → 继续装配
  │    └─ sync: require-local → fail fast(为打包期物化预留的收紧档)
  └─ 哈希不符 → fail fast(篡改/漂移,永不静默重下)
```

- 首次启动付一次下载成本,之后 `.skills` + lock 就位,重启零网络;
- 下载失败 fail fast(不留半个 app),错误信息带 ref 与原因;
- 装配是串行按声明序拉取,包数通常个位数,不做并发复杂度。

### 6.2 打包期物化(延后)

核心同步逻辑从第一天就是库函数 `config.SyncSkillpacks(spec, dir, opts)`
(启动期就是在调它)——将来补一个薄壳 CLI(`agentkit skills
sync/verify/list`)与 Dockerfile 样例即可,应用与配置零改动,生产切
`require-local` 完成收紧。此项延后不影响 v1 任何能力。

## 7. 装配与执行(框架内,非转发)

### 7.1 skill.BuildPack(组合核心,落在 skill 包)

- **Meta**:`Ref{Kind: "skillpack", Domain: ns, Name, Version}`;`Tags`
  记来源 ref 与内容 sha(观测/回溯)。
- **工具面**(最小权限,三层交集:allowed-tools ∩ 条目 tools ∩ 目录):
  1. `pack_read`:包目录只读(路径囚笼,防 `../` 逃逸)——L3 读取口;
  2. exectool caps:包含脚本时绑定(工作目录 = 该包的 .skills 子目录),
     沙箱引擎沿用 `protocol/exec` 的 RegisterEngine(框架不拥有隔离);
  3. 目录内已有能力按白名单选入。
- **提示词**:L2 = SKILL.md 正文,选中才注入内部 react 循环;宿主上下文
  只见 L1 描述。
- **Ring 0**:把 skill.Build 现有闸门栈抽成包内共享 `applyGates`
  (超时→消化→截断→效果日志→审批),Build/BuildPack 同源,治理不分叉;
  内部模型调用经 BudgetModel 计入宿主会话预算(现有 ctx 机制)。
- **风险**:含脚本 → `RiskDangerous`(目录准入需显式 `max_risk:
  dangerous`,live 冒烟已验证这道闸真实拦截);纯指令 → 按白名单工具
  最大风险传播。

### 7.2 分层落位

分层规则 7(守卫脚本在测):impl 只准依赖 core/protocol;pack 组合需要
runtime/engine + runtime/loop → **组合与 fetch 都落在 skill 包(L3)**
(`skill/pack.go`、`skill/fetch.go`,fetch 仅 stdlib:net/http +
archive/zip)。CLI 在新顶层 `cmd/agentkit`(与 examples 同级消费者,
import config,不受 L1-L4 约束)。v1 不做 `type: skillpack` 的 bulk
source 形态(会迫使 impl 越层),需求出现再议。

## 8. 与 Claude Code Skills 的机制对比

发现与格式对齐,执行模型不同——这是有意的设计选择,不是实现差距。

| 维度 | Claude Code | agent-kit skillpack | 评注 |
|---|---|---|---|
| 技能格式 | SKILL.md(frontmatter + 正文 + 打包脚本) | **同一格式**,直接兼容 | 吃同一个生态 |
| 渐进披露 | L1 描述常驻 → 相关才读正文 → 按需读文件/脚本 | **同一哲学**,L1/L2/L3 同构 | 一致 |
| 触发方式 | 模型从描述清单判定相关(或 /命令显式调) | 大脑从目录选品,**以工具调用形态触发** | 机制等价,形态不同 |
| **执行位置** | **主循环内联**:正文注入当前对话,主 agent 直接照做 | **隔离子循环**:正文只进 pack 自己的 react 循环,宿主只见 L1 与最终结果 | 核心分歧点,见下 |
| 上下文可见性 | 技能天然看得到全部对话历史 | 默认隔离;需要背景时走现有 fork 快照机制(损失可控地带入) | 各有得失 |
| 工具面 | 主 agent 全部工具,allowed-tools 生效期收窄 | **默认零工具**,三层白名单交集才给 | 我们更严 |
| 脚本执行 | Bash 跑在用户环境,权限系统逐条把关 | exectool + 可插拔沙箱引擎,工作目录囚笼 | 场景不同:本机工具 vs 服务端框架 |
| 治理 | permissions/hooks | Ring 0 全套(超时/消化/截断/审批/预算),与内部能力同源 | 我们统一在框架层 |
| 获取分发 | 本地目录/插件市场安装 | `use:` 链接 + 启动期下载 + `.skills`/lock 供给链锁定 | 我们面向部署,有 lock |
| 上下文污染风险 | 技能指令可影响主对话后续行为 | 结构性隔离,pack 无法改写宿主 persona | 服务端多租户的硬要求 |

**为什么执行位置选隔离而非内联**:Claude Code 是**单用户本机工具**——用户
亲自监督,技能指令融入主对话是特性(技能教主 agent 做事);agent-kit 是
**服务端框架**——外部技能是不受信供给链,必须有上下文边界(prompt
injection 不能穿透宿主)、权限边界(默认零工具)、预算边界(计入宿主但可
审计)。这恰好是现有 skill 三边界(接口/上下文/权限)的直接复用,也是
"skillpack = react component"这个映射的根本理由。

**代价与补偿**:内联模式下技能可直接利用对话上下文;隔离模式默认看不到。
补偿:pack 条目可声明 `context: fork`(与编排步骤的既有字段同名同义,
复用 loop.ForkMessages 会话快照机制,把调用方对话背景带进子循环)——
按需付费,默认关闭(fresh)。

## 9. 安全模型(供给链视角)

| 威胁 | 防线 |
|---|---|
| 链接内容被替换 | pin 默认强制 + integrity/lock 哈希 + 启动期校验;漂移 fail fast 永不静默重下 |
| 供给链变更不可见 | skills.lock 提交 git,变更走 code review(对齐 go.sum) |
| 恶意指令(prompt injection) | L2 正文只进 pack 自己的内部循环;宿主只见 L1 描述 |
| 恶意脚本 | 只经 exectool + 可插拔沙箱;`RiskDangerous` 过准入;审批规则可按 `cap://skillpack/*` 定 deny/ask |
| 越权用工具 | 三层白名单交集,默认拿不到任何宿主工具 |
| 文件逃逸 | pack_read 路径囚笼;exec 工作目录 = 包目录 |
| 运行期出网 | 结构性禁止:fetch 代码只在装配/CLI 路径可达 |
| 预算失控 | 内部调用计入宿主会话预算(BudgetModel) |

## 10. 实施批次

| 批 | 内容 | 量 | 验收 |
|---|---|---|---|
| 1 | fetch/verify/lock(skill/fetch.go):file:/https zip/github 短写、树哈希、skills.lock 读写 | 中 | 离线 fixture(testdata zip + file:):pin/未pin/坏hash/漂移 各 fail fast 路径 |
| 2 | SKILL.md 解析 + `skill.BuildPack` 纯指令子集 + `applyGates` 抽取 | 中 | fixture pack 经 testmodel:L1 进目录、L2 注入、白名单交集、Ring 0 生效 |
| 3 | config 接线:SkillEntry.Use、skillpacks 块、启动期 resolveLocal(auto/require-local) | 中 | smoke 树加 file: fixture pack;require-local 缺失 fail fast 断言 |
| 4 | 脚本支持:runtimes 检测 + exectool 绑定包目录 + pack_read | 中 | fixture 带 python 脚本;live 冒烟新增子测试(真实 MiniMax 调 pack 脚本,复用 dangerous 准入模式) |
| 5 | 文档 + examples(引用真实公开 SKILL.md 仓库) | 小 | README/examples 注释 |
| 延后 | cmd/agentkit skills sync/verify/list 薄壳 CLI + Dockerfile 样例 | 小 | CLI 端到端:sync → verify → 改字节 → 报漂移 |

批 1-3 离线全绿即纯指令包可用;批 4 接脚本(exectool 已在)。

## 11. 决策点(待拍板)

| 决策 | 选项 | 建议 |
|---|---|---|
| 打包期 CLI | 已拍板:**延后**,v1 只做启动期下载安装(库函数留口) | — |
| cap kind | 已拍板:**复用 `skill`**,与内部一致(溯源在 Tags,治理靠 Risk) | — |
| lock 提交策略 | a) 建议提交 git(评审可见);b) 只进镜像 | **a**,与 go.sum 同待遇 |
| 启动期默认策略 | a) auto(开发友好);b) require-local | **a** 为默认,文档明确生产镜像配 b |
| git 拉取 | a) codeload zip(零依赖);b) git 二进制 | **a**;私有仓走内部 https/file: |
| .skills 进 git | 内容目录默认 .gitignore(lock 必提交),想全 vendor 的团队自行提交 | 折中:锁事实 + 不膨胀仓库 |
