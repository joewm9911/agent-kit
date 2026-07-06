# 脚本执行沙箱化方案

> 状态:**已落地**(批 A-E,见对应 commit)。收口三件事:命名整改、可强制
> 的沙箱默认、开箱即用的官方运行环境。

## 0. 背景

skillpack / exectool 让模型跑脚本(pdf 写 pypdf、superpowers 带 .sh/.js)。
原执行原语 `protocol/exec` 三级解析(engine > command > 宿主直跑)有三个问题:
`engine` 与编排引擎(runtime/engine)命名撞车;不配就宿主裸跑;没有开箱即用
的运行环境(依赖要用户在宿主自己装)。

## 1. 命名整改(批 A)

`exec.Engine`→`exec.Sandbox`,`RegisterEngine`→`RegisterSandbox`;exectool 配置
`engine`/`engine_config`→`sandbox`/`sandbox_config`。规范性命名:这个槽就该放
隔离实现,宿主直跑是需显式 justify 的例外。

## 2. 强制沙箱(批 B)

四级解析:`工具级 sandbox > 工具级 command > 装配层默认 sandbox > 内置模板
(宿主直跑)`。默认是 app 级策略 `config.ExecConfig`(default_sandbox /
sandbox_config / require_sandbox),**装配期解析后注入进各 exec source 的 conf,
不是 exec 包全局单例**(守 store/budget 清理时立的 DI 线:消费方持有被注入的
东西,不读全局)。`require_sandbox` 禁用最后一级——无沙箱可用的 exec 工具
装配即 fail fast,脚本裸跑架构上不可能。

## 3. 官方运行环境(批 C)

`examples/engines` 的 docker 升格为 `impl/exec/docker`,同 impl/store/redis
模式:核心零依赖,空导入 + `default_sandbox: docker` 即得加固运行环境
(--rm、无网络、只读根、tmpfs、cap-drop=ALL、no-new-privileges、内存/CPU/PID
上限)。python-only 扩展为多 runtime(python3 -c / node -e / bash -c / sh -c),
runtime 由 exec 工具经 sandbox conf 注入。配套 `examples/exec-runtime/Dockerfile`
预装 pypdf/pdfplumber/nodejs——依赖在镜像层解决,不碰宿主、不靠模型临场装。

## 4. skillpack 透传(批 D)

`buildSkillpack` 收 app 级 ExecConfig 并注入 pack 的 exec source——pack 脚本
从"只能宿主直跑"变为"随 exec.default_sandbox 进沙箱";两条 build 路径(平铺
skills / namespace skills)都透传。

## 5. 安全模型

脚本在哪跑由装配期注入的 Sandbox 绑定死,不在模型可达路径:模型临场
`pip install`/shell 连同脚本都在 Sandbox 里跑,宿主不可见;`require_sandbox` +
无宿主兜底 = 脚本裸跑被禁。叠加既有防线:脚本包 Dangerous + 目录准入 + 审批
闸门 + 预算。

## 6. 交互用例(批 E)

`examples/superpowers` 的交互 runner:三个 superpowers 真实技能作研发教练,
真实 MiniMax 路由。生产沙箱化只需 `impl/exec/docker` 空导入 + `exec:
{default_sandbox: docker, sandbox_config: {image: agent-kit-runtime}}` +
镜像预装依赖。

## 7. 生产配置示例

```yaml
exec:
  default_sandbox: docker
  sandbox_config: {image: "agent-kit-runtime:latest", network: none, memory: 512m}
  require_sandbox: true      # 禁止宿主直跑
```

```go
import _ "github.com/joewm9911/agent-kit/impl/exec/docker"
```
