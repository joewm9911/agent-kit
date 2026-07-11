# 开源就绪评估与优化方案

> 状态:待 review。视角:**第三方使用者**按开源标准审视本仓库——他们
> 没有上下文、不认识维护者、用五分钟决定采用或关闭标签页。
> 评估维度:工程基线 / API 面与目录 / 架构合理性 / 可扩展性 / 语言策略 /
> 文档体系 / 安全 / 测试质量。每项给判定与优先级;P0 = 发布一票否决项,
> P1 = 第三方可用性,P2 = 打磨与登记。

## 0. 现状审计总表

| 维度 | 现状 | 判定 |
|---|---|---|
| LICENSE | **无** | 🔴 P0-1:法律上第三方不可用,一票否决 |
| CI 门禁 | **无 .github**;test/vet/layering-check 全靠手跑 | 🔴 P0-2 |
| 版本与变更 | 无 tag、无 CHANGELOG;pre-1.0 硬切文化 | 🔴 P0-3 |
| module path | `github.com/joewm9911/agent-kit` 个人名下 | 🟡 P0-4 裁决 |
| 仓库卫生 | 顶层混入运行产物目录(data/ interactive/ agent-kit/ superpowers/,gitignore 覆盖但污染 checkout 观感);examples/examples 嵌套残留 | 🟡 P0-5 |
| README/文档语言 | 全中文;docs/ 18 篇全是内部设计与已执行完的计划文档 | 🟡 P1-1/P1-2 |
| 错误消息语言 | 279 处 fmt.Errorf 中 79 处含中文(28%) | 🟡 P1-3 裁决 |
| 引擎内置提示词 | rewoo/plan-execute/reflection/router 共 8 条中文 | 🟡 P1-4 裁决 |
| 面向用户硬编码文案 | serving 层「处理失败:」「消息太多啦」「⏳ 处理中...」等 | 🟡 P1-5 |
| 通道覆盖 | 仅 feishu;Channel 接口干净但无第二个参考实现 | 🟡 P1-6 |
| observability 扩展 | 无注册表,内置两开关,第三方平台走宿主代码挂全局回调 | 🟡 P1-7(已登记项) |
| 分层与 DI | core→protocol→runtime→L3→serving→config;脚本守卫;注册表 12+ 个 | 🟢 强项,保持并写进文档当卖点 |
| 装配期 fail fast | 报错内嵌新写法(迁移指南即报错文本) | 🟢 强项 |
| Ring 0 门链/挂起重放 | harness 强制纪律、durable HITL | 🟢 强项,差异化卖点 |
| 全局状态 | eino 全局回调 + sync.Once 进度切面:同进程多 App 事件串台 | 🟠 P2-1(已知登记) |
| 注册表冲突面 | 全局 map + 重名 panic,无命名空间约定,跨第三方库有冲突可能 | 🟠 P2-2 |
| API 稳定性分层 | 除 internal/testmodel 外全导出,承诺面 = 全部符号 | 🟠 P2-3 |
| 宿主注入面 | 分散:BuildOptions / serving.RegisterDecorator / redisconn.RegisterClient / local.Func…无统一索引 | 🟡 P1-8(文档解法) |
| 安全基线 | secrets provider、webhook 验签、sandbox require_sandbox 齐;无 SECURITY.md | 🟡 P0-6(仅补文档) |
| 测试 | 114 源/76 测试文件;live 测试 SMOKE_LIVE 门控;testmodel 回放 | 🟢 结构好,缺 CI 承载 |

**架构合理性总评**:分层、DI、注册表、错误两类、装配 fail fast 这套骨架
按开源标准是**超标**的(多数同类项目没有分层守卫和注册表纪律)。需要
优化的不是架构本身,而是:①架构的**可发现性**(第三方看不到这些纪律,
它们只存在于中文注释和 git 历史);②少数**单使用方假设**(语言硬编码、
全局切面、feishu 独苗)。

## 1. P0 —— 发布门槛(不做完不能公开)

### P0-1 LICENSE
裁决项:Apache-2.0(推荐,专利条款对企业采用友好,与 eino 同族)或 MIT。
**前置动作(仓库外)**:确认 IP 归属(个人作品 vs 雇主工作产出),这是
整个方案的总开关。

### P0-2 CI 门禁(GitHub Actions)
单 workflow 四个 job:`go build ./...`、`go test ./... -race`(live 测试
天然被 SMOKE_LIVE 门控跳过)、`go vet` + 格式检查、`scripts/layering-check.sh`
(分层纪律从"维护者自觉"升级为"机器强制",这本身就是对外卖点)。
README 挂徽章。

### P0-3 版本纪律
打 `v0.1.0`;新增 CHANGELOG.md(Keep a Changelog 格式)。**硬切文化与
开源的和解方案**:v0.x 期间破坏性变更允许,但必须①CHANGELOG 的
Breaking 段落列出;②保留"报错文本内嵌新写法"的既有纪律(这是比多数
项目的 deprecation warning 更好的迁移体验,写进 CONTRIBUTING 当规范)。

### P0-4 module path(裁决)
个人名下 `joewm9911/agent-kit` 可以发布,但迁移 org 会破坏所有 import。
**建议现在定终身路径**:要么确认长期个人维护留现名,要么建 org
(如 `agentkit-go/agent-kit`)一次到位。发布后再迁 = 强迫所有用户改代码。

### P0-5 仓库卫生
- 运行产物目录(data/ interactive/ agent-kit/ superpowers/)统一挪到
  `.agent-kit-work/` 之类单一前缀,或 examples 默认 work_dir 指到系统
  临时目录——checkout 后顶层只该有代码;
- 清理 examples/examples 嵌套与 product_catalog.html 等业务残留;
- 顶层 20 个目录对新人过载:L3 能力包(agent/skill/todo/askuser)收进
  `capabilities/` 或保持现状但 README 给目录地图(**推荐后者**,改目录
  是大动作,收益主要是观感)。

### P0-6 社区三件套
CONTRIBUTING.md(含分层规则、注册表模式、错误两类、真机验证纪律——
把记忆里的设计原则文档化,贡献者照着写)、SECURITY.md(漏洞报告渠道 +
"密钥永不入库"守则)、issue/PR 模板(最小)。

## 2. P1 —— 第三方可用性

### P1-1 README 与文档体系重构
- README 双语(英文为主、中文全文在 docs/ 或 README.zh.md);
- 新增 docs 使用者线:`quickstart.md`(5 分钟单文件 YAML 跑通)、
  `architecture.md`(分层图 + 设计原则,现成素材)、`extending.md`
  (见 P1-8)、`configuration.md`(以 config-taxonomy 词汇表为底);
- 现有 18 篇内部计划文档移 `docs/design/`(历史价值保留,与使用者
  文档分流;已执行完的 plan 文档标注"已实施,存档")。

### P1-2 杀手级示例前置
examples 重排:`examples/quickstart`(无外部依赖,testmodel 或
openai 兼容端点)→ `examples/interactive`(现有,标注需要 minimax/飞书)
→ `examples/durable-approval`(挂起审批跨重启,配 README GIF——
推广方案里的核心 demo 同时是文档)。

### P1-3 错误消息语言(裁决)
错误消息是 API 的一部分,第三方拿它进日志/监控/搜索引擎。三选一:
- **A. 全英文化**(79 处改写,推荐):国际开源标准做法;
- B. 保持中文:等于宣告"目标用户仅中文圈"——与飞书定位自洽,但天花板锁死;
- C. 双语:维护成本翻倍,不推荐。
建议 A,且新增 CONTRIBUTING 规则:错误消息/导出符号 doc 注释英文,
实现注释中文随意(注释是维护者的,报错是用户的)。

### P1-4 引擎内置提示词语言(裁决)
中文提示词会把国际用户的模型输出带偏中文。方案:默认改英文(与 L1
循环规约一致),中文版本保留为文档示例(prompts 本就可配置覆盖,
`engine_config.planner_prompt` 一行换回)。真机对照(engine_live_test)
两种语言各跑一轮,确认英文版格式遵循不回退。

### P1-5 用户可见文案可配置
serving 层硬编码中文(占位卡、错误前缀、限流提示、中断确认、挂起等待
「⏸ 已向你提问」)收敛为 Binding 级文案表(map + 内置默认),配置可覆盖
——这同时解决"第三方想改措辞"和"语言"两个问题,改动小。

### P1-6 第二个通道参考实现
仅有 feishu 时,Channel 接口的通用性是**未经证明的声明**。补一个
`impl/channel/webhook`(通用 HTTP 进出,POST 入站 + 回调出站,~150 行):
①最小参考实现,第三方照抄接自己的 IM;②自动成为 CI 里通道层的
无外部依赖测试载体。slack/telegram 登记不做(有 webhook 样板后
第三方自己能接)。

### P1-7 observability 注册表
既有登记项升级为 P1:`observe.Register(name, builder)` + 配置
`observability.handlers: [name]`,Langfuse 从宿主代码示例升级为
`impl/observe/langfuse`(依赖已在 go.mod)。这是第三方最常问的
"怎么接我们的监控"的配置化答案。

### P1-8 扩展点总索引(文档)
(未建)`docs/extending.md` 一张表收全 12+ 注册表:扩展什么 → 实现哪个接口 →
在哪注册 → 配置怎么引用 → 参考实现在哪。现状这些散在各包注释里,
第三方无从发现"原来 model/store/channel/engine/decorator/redis 客户端
全都可换"。这张表就是"可扩展性"的证明书。

## 3. P2 —— 打磨与登记

### P2-1 多 App 进程隔离(登记,暂不做)
eino 全局回调决定了观测/进度切面是进程级;同进程多 App 场景(库形态
嵌入宿主)事件会串台。解法是 per-invocation callback 包装,改动面大,
单 App 进程(当前所有场景)无痛。文档声明限制即可。

### P2-2 注册表命名空间约定
跨第三方库空导入可能重名 panic。约定 `<vendor>/<name>` 命名(如
`corp/redis`),内置实现无前缀。文档约定即可,不改代码。

### P2-3 API 稳定性分层
v0.x 期间用文档声明:config YAML 面 + protocol 接口 + 注册函数 =
稳定面;runtime 内部(loop/engine 的具体函数)= 无承诺面。1.0 时再
考虑 internal/ 物理隔离(现在做会打断 impl 的自由引用,得不偿失)。

### P2-4 provider 覆盖
anthropic 原生 provider(当前 openai 兼容面可绕但非原生);登记,
有用户要再做。

## 4. 实施批次

| 批 | 内容 | 规模 |
|---|---|---|
| 0(仓库外) | IP 确认、LICENSE 选择、module path 裁决 | 决策 |
| 1 | P0-1/2/3/5/6:LICENSE + CI + tag/CHANGELOG + 卫生 + 三件套 | 1 天 |
| 2 | P1-3/4/5:错误消息英文化 + 引擎提示词英文默认(真机对照)+ 文案表 | 2-3 天 |
| 3 | P1-1/2/8:README 双语 + 文档体系 + quickstart/durable-approval 示例 + 扩展索引 | 2-3 天 |
| 4 | P1-6/7:webhook 通道 + observability 注册表 | 1-2 天 |
| 5 | P2 文档声明项(稳定面、命名空间、多 App 限制) | 半天 |

批 2 是唯一动核心代码的批次(报错文本改写会牵动断言这些文本的测试,
硬切一次做完);其余批次几乎全是增量,不影响现有使用方。

## 5. 需要你裁决的清单

1. **IP 归属确认**(总开关);
2. LICENSE:Apache-2.0(推荐)vs MIT;
3. module path:留个人名 vs 建 org 一次到位;
4. 错误消息:英文化(推荐)vs 保持中文(=只做中文圈);
5. 引擎提示词:英文默认 + 中文可覆盖(推荐)vs 保持中文;
6. 顶层目录:保持 + README 地图(推荐)vs 收拢 L3 进 capabilities/。
