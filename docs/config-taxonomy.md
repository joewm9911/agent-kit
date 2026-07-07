# 配置标签语义治理方案

> 状态:已实施(2026-07,批 1-4 全部落地;本文转为配置标签规范)。目标:每个标签**一词一义**,同一语义
> **全库同词**;歧义要么改名消灭、要么以"保留键/前缀"结构化表达。
> 迁移策略:项目 pre-1.0 且单一使用方,**硬切不留双轨**——旧写法
> 装配期报错并在报错文本里给出新写法(fail fast 即迁移指南)。

## 0. 全量审计结论

| 标签 | 出现位置 | 语义 | 判定 |
|---|---|---|---|
| `use` | graph step / skill 入口引用 | 引用能力(tools/·components/·model·cap://) | ✅ 保留 |
| `use` | SkillEntry(skillpack) | **外部包获取链接**(github/https/file) | ⚠️ P0-1 改名 |
| `params` | skill/component/graph | 形参声明(对外接口,type/desc/required → JSON Schema) | ✅ 保留 |
| `args` | graph step / approval rules | 实参(使用点的调用模板 / 参数匹配模式) | ✅ 保留 |
| `prompt` + `{ref:}` | agent/component/compaction/engine_config | 字面量或引用映射双形态 | ⚠️ P0-2 统一裸字符串 |
| `ref` | step args 映射保留键 | 模板引用源 | ⚠️ P0-2 随上项收编为 `use` |
| `tools` | NamespaceConfig | **源声明**([]SourceConfig) | ⚠️ P0-3 改 `sources` |
| `tools` | ComponentConfig | 工具面引用([]string) | ✅ 保留 |
| `tools` | SkillEntry | 白名单收紧(∩ allowed-tools) | ✅ 保留(仍是"工具面"语义) |
| `tools` | MemoryConfig | **开关 bool**(挂载记忆读写工具) | ⚠️ P1-2 语义漂移 |
| `context` | step / SkillEntry | fresh(默认,从零)\| fork(**快照**) | ⚠️ P1-1 与下行冲突 |
| `context` | skillpack frontmatter | fork(**隔离**)\| fork_with_context(快照) | ⚠️ P1-1 同词反义 |
| `max_steps` | Profile.Loop | 实际语义是**轮数**(一轮=一次决策+一批工具) | ⚠️ P2-1 名实不符 |
| `store` 槽 | session/todo/digest/... | 裸 type 或 cap://store/... 字符串双形态 | ✅ 既定前缀识别模式,文档化 |
| `type` | sources/stores vs params | 实现选择 vs 数据类型 | ✅ 语境隔离,不动 |
| `model` | use: model vs model: 块 vs models: | 步骤引用 vs 执行画像 vs 具名清单 | ✅ 语境隔离,不动 |
| `output` | graph 出口 vs structured_output | 步骤名 vs schema 块 | ✅ 不动 |

## 1. P0(三件,语义硬伤,立即切)

### P0-1 `use` 单一化:引用能力;外链改 `from`

`use` 的正当语义是"**使用一个已存在的能力**"(引用词汇表:tools/、
components/、model、cap://)。skillpack 的外部链接是"**从哪获取**",
是 fetch 语义不是引用语义,改 `from`:

```yaml
skills:
  - name: pdf
    from: github.com/anthropics/skills/skills/pdf@9d2f1ae...   # 原 use:
    integrity: sha256:...
```

### P0-2 提示词统一裸字符串,`cap://` 前缀自动识别,删除 `{ref:}` 映射

框架已有先例:store/retriever 槽就是"裸 type 或 cap:// 前缀"的
字符串识别——`{ref:}` 映射反而是全库唯一的异类。统一后:

```yaml
prompt:
  system: "你是运营助手..."                      # 字面量
  # system: "cap://prompt/pp/assistant-persona"  # 引用(前缀识别,@label 照旧)
compaction:
  prompt: "cap://prompt/pp/ops-summarize"
engine_config:
  planner_prompt: "cap://prompt/pp/planner"
```

step args 联动:标量以 `cap://prompt/` 开头 = 纯引用;**带绑定的映射
形态,模板源键由 `ref` 改为 `use`**(与步骤级 `use` 同义:使用这个模板):

```yaml
- name: answer
  use: model
  args: "cap://prompt/fornax/oec.fulfillment.insight_mask"     # 无绑定:标量即可
- name: answer2
  use: model
  args:
    use: "cap://prompt/fornax/oec.fulfillment.insight_mask"    # 带绑定:use + 绑定键
    analysis: "{analysis}"
```

误伤面评估:字面量提示词以 `cap://prompt/` 开头的概率为零;万一需要,
留内部转义不做配置面(YAGNI)。`prompt.Value` 类型保留(Literal/Ref
内部表示不变),只改 UnmarshalYAML;`{ref:}` 旧写法报错并附新写法。

### P0-3 NamespaceConfig `tools:` → `sources:`

它声明的是**能力供给源**(name/type/config,与顶层 `sources:` 完全
同构),不是工具列表。改名后"声明源用 sources、引用工具面用 tools"
全库一致:

```yaml
namespaces:
  - name: catalog
    sources:                       # 原 tools:
      - {name: shop, type: http, config: {...}}
    components:
      - name: analyst
        tools: ["tools/shop/*"]    # 引用语义的 tools 不变
```

## 2. P1(两件,语义陷阱)

### P1-1 `context` 枚举全库统一

现状是同词反义:step/本地 `fork` = 带调用方快照;pack frontmatter
`fork` = 完全隔离、`fork_with_context` = 带快照。统一词表(以使用面
更广的 step 语义为准):

| 值 | 语义 | 适用位置 |
|---|---|---|
| `fresh`(默认) | 完全隔离,从零起步 | step / SkillEntry / frontmatter |
| `fork` | 带调用方对话快照(背景非指令) | 同上 |

frontmatter 的 `fork`(隔离)→ 改写为 `fresh`;`fork_with_context`
→ `fork`。外部包兼容:frontmatter 是第三方文件,**保留旧值解析**
(fork→fresh、fork_with_context→fork 映射)但装配日志 warn 提示;
本地配置(SkillEntry/step)无兼容负担,直接新词表。

### P1-2 MemoryConfig `tools: true` → `tools_enabled` 或并入能力开关

bool 开关顶着"工具列表"惯用词。改 `expose_tools: true`(或收进
capabilities 开关族,与 `ask_user` 同型)。倾向后者,与既有
`capabilities.ask_user` 形成"内置能力开关"一族。

## 3. P2(一件,名实对齐)

### P2-1 `max_steps` → `max_rounds`

内部早已是轮数语义(BuildReAct 换算 rounds*2+1),标签还叫 steps。
改 `max_rounds`;`steps:`(执行画像的步骤默认块)与图编排的 `steps:`
不受影响(那两处名实相符)。

## 4. 文档化项(不改码)

- `store`/`retriever` 槽的"裸 type 或 cap://"前缀识别写进 schema 注释与示例;
- component 的 `prompt:` 实为**任务书模板**(渲染为子循环的 user 消息,
  非 system persona)——语义与 agent 的 prompt.system 不同但改名收益低,
  注释与文档明示;
- `args` vs `params` 的形参/实参定义写进 schema 头注释。

## 5. 实施批次与迁移

| 批 | 内容 | 破坏面 |
|---|---|---|
| 1 | P0-2 提示词裸字符串(prompt.Value 解析 + step args 的 ref→use)| 全库示例/测试同步改;旧 `{ref:}` 报错含新写法 |
| 2 | P0-1 `from` + P0-3 `sources` | 同上,报错指路 |
| 3 | P1-1 context 词表 + P1-2 memory 开关 | frontmatter 留兼容映射 + warn |
| 4 | P2-1 max_rounds + 文档化项 + prompt-inventory/examples 全量同步 | — |

每批:schema/解析 → 全库示例与测试同步 → 旧写法报错文案(含新写法
示例)→ 单测覆盖新旧两路(旧路断言报错文本)。全部完成后
docs/config-taxonomy.md 转为"配置标签规范"长期文档(词汇表章节保留)。

## 6. 定版后的词汇表(治理完成态)

| 词 | 唯一语义 |
|---|---|
| `use` | 使用/引用一个能力或模板(引用词汇:tools/、components/、model、cap://) |
| `from` | 外部获取来源(skillpack 链接) |
| `sources` | 能力供给源声明(顶层与 namespace 同构) |
| `tools` | 工具面(引用集合或其白名单收紧) |
| `params` | 形参声明(对外接口 schema) |
| `args` | 实参(使用点模板/匹配模式) |
| `prompt` | 提示词(字面量或 cap://prompt/ 前缀引用) |
| `context` | 子循环上下文起点:fresh(隔离)\| fork(带快照) |
| `max_rounds` | 循环轮数上限 |
