# superpowers 冒烟场景

用 [obra/superpowers](https://github.com/obra/superpowers)(Claude Code 生态
最复杂的技能集之一)验证外部 skillpack 的真实生态兼容:同一仓库 pin 一个
commit 拉三个技能,一次装配覆盖三种形态——

| 技能 | 形态 | 检测到的风险 |
|---|---|---|
| brainstorming | 多文件 + .sh/.js 脚本(bash+node) | Dangerous |
| systematic-debugging | 重参考文档(9 个伴生 md)+ .sh | Dangerous |
| test-driven-development | 纯指令 md | Readonly |

冒烟(config 包 TestLiveSuperpowers)硬断言:

- 三包物化进固定目录 `<PROJECT_WORK_DIR>/agent-kit/.skills` + skills.lock;
- 风险按包内容自动分级(脚本包 Dangerous 须 `catalog.max_risk: dangerous` 准入);
- 真实 MiniMax 的双技能路由:排障任务 → systematic-debugging(轨迹含其
  Tool span 与子循环内 **pack_read** span——L3 渐进披露,模型先读
  root-cause-tracing.md 再作答),写测试任务 → tdd(轨迹含其 Tool span);
- 轨迹留档 work/superpowers-trajectory.jsonl 供人工复核。

运行:

    MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
    SMOKE_LIVE=1 go test ./config/ -run TestLiveSuperpowers -v -count=1

环境占位:SMOKE_MODEL_PROVIDER / SMOKE_MODEL_BASE / MINIMAX_API_KEY /
SKILLPACK_WORK_DIR / SUPERPOWERS_TRAJ(由测试注入)。中间产物
(work/、宿主根 agent-kit/.skills)保留且 gitignore。

## 交互版

`interactive.yaml` + `main.go` 是同一技能集的**可交互副本**(smoke.yaml 保持纯
测试用途)。真实 MiniMax,coach agent 按你的问题路由到 brainstorming /
systematic-debugging / tdd:

    MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
    go run ./examples/superpowers

首启从 GitHub 下载三个技能到 <work_dir>/agent-kit/.skills(之后零网络)。带
脚本的技能是 Dangerous 风险,调用时终端请求审批(y 放行,a 本会话免问)。
