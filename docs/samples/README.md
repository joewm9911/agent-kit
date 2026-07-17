# 上下文样本

一次真机模型调用的**完整请求体**——从 HTTP 出口拦截的原始字节(非二次
序列化),用于对照 docs/context-architecture-plan.md 的上下文结构。

- `model-request-sample.raw.json` — eino openai provider 真正 POST 给
  MiniMax 的原始 JSON(13.8 KB,逐字节);
- `model-request-sample.json` — 同一内容 pretty-print(缩进 + 不转义中文)。

场景:examples/smoke 的 ops-manager 第 1 轮「帮我审查P100的定价」,
模型 MiniMax-M2.7。结构 `{model, messages, tools, tool_choice}`:

- **messages**(3):system 提示词(L1 规约 + L2 persona + L3 环境)、
  user、Focus 重述——历史注入一律 `<system-reminder source=…>` 信封;
- **tools**(17):skill(过程卡)/ sub-agent / 内置工具 / 直挂工具**同构
  排列**,统一 `{type:"function", function:{name, description, parameters}}`
  信封——模型看不到"skill 还是 tool"的类型区别,只看描述决策;skill 的
  正文不在这里(两级披露:描述进 tools,指引在调用后作为结果返回)。

复现(需 MINIMAX_API_KEY):见 git 历史中抓取用的临时代理测试
(起本地录制代理转发到 MiniMax,拦第一条 chat/completions body)。
