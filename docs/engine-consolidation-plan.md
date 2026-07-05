# 引擎收口:graph/workflow 挪进 engine/

## 决策
`engine:` 配置面已经把 graph/workflow 当引擎选项(编排族),但实现却在
`skill/graph.go`,与循环族(`engine/`)分居两处。**把编排引擎 graph/workflow
挪进 `engine/`,让 `engine/` 收口全部引擎**(循环族 + 编排族),和配置面一致。

## 为什么现在卡在 skill
`skill/graph.go` 用了 skill.go 的两个符号:
- `ParamDecl`(参数声明 `{type,desc,required}`)
- `paramsSchema(params) → schema.ParamsOneOf`(参数→入参 schema)

关键认识:**这俩是「可调用单元的参数接口」,通用的**——循环族 skill
(`skill.Build`)、编排族 skill(`BuildGraph`)、config 的
ComponentConfig/NamespaceSkill 都用它。它不是 skill 私有,只是恰好定义在
skill.go。`Step`/`GraphDeclaration`/`StepResolver` 同理,是通用「编排声明」。

所以 graph 依赖 skill 是**错误的归属**,不是本质耦合。

## 目标依赖结构
```
capability(基座) ← engine(全部引擎 + 编排声明) ← skill(装配 skill) ← config
```
- `ParamDecl` + `paramsSchema` → 下沉到 **capability**(可调用单元的参数
  接口,属于基座;capability 已有 SingleParam/ParamsOneOf 等同类 helper)。
- `Step` / `GraphDeclaration` / `StepResolver` / `BuildGraph` / `compileGraph`
  + 内部类型 → 挪到 **engine/graph.go**(编排引擎)。workflow 的「纯顺序、
  禁 needs」约束一并带过(现在散在 namespace.go)。
- `skill.Build`(循环族)改用 `capability.ParamDecl`;不再拥有 graph。
- `config/namespace.go` 的 `skill.BuildGraph(...)` → `engine.BuildGraph(...)`。

无环:capability 谁都能 import;engine→capability;skill→engine/capability;
config→全部。

## 实施步骤(待动手,记录用)
1. `ParamDecl` + `paramsSchema` 从 skill.go 移到 capability(改名
   `capability.ParamDecl` / `capability.ParamsSchema`);全项目引用改指。
2. graph 编排类型 + 执行器(Step/GraphDeclaration/StepResolver/BuildGraph/
   compileGraph/graphPlan/…)从 skill/graph.go 移到 engine/graph.go。
3. skill/skill.go(循环族)引用改为 capability.ParamDecl;删对 graph 的依赖。
4. config/namespace.go:`skill.BuildGraph` → `engine.BuildGraph`;workflow 的
   顺序校验保留在 config 层或随执行器进 engine。
5. skill.Declaration.Kind 机制、component 升 kind 等不变。
6. 迁移测试(skill/graph_test.go → engine/graph_test.go)+ 全仓 -race。

## 收益
- `engine/` = 唯一「执行引擎」收口处,和 `engine:` 配置面一一对应;读代码
  找引擎只看一个包。
- graph/workflow 归位为编排引擎,不再「寄居」skill。
- 通用声明(params/steps)下沉到该在的层(capability/engine),各引擎与
  skill 平等复用。

## 时机
独立于 config 治理大重构(统一 Profile);可在其前后单独做。纯归属搬迁 +
少量改名,零行为变更,靠编译器兜底。
