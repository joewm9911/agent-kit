// Package loop 提供主循环的运行时保障(Ring 0):提示词分层拼装、
// 上下文压缩、预算控制、审批拦截、结构化输出。这些是"模型没得选的
// 规则"——如果做成工具,模型可以选择不用,保证就不存在了。
package loop

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/engine"
)

// L1 框架规约:大脑的元规则,只讲档位选择与运行纪律,不含任何业务逻辑。
// 文本直接移植 Claude Code 系统提示词的运行纪律章节(Tone and style /
// Proactiveness / Task management / Tool usage / Completion),做了三处
// 必要适配:工具名替换为本工具面(todo_write/ask_user/skill 工具),
// 编码专属章节(git/lint/CLAUDE.md)裁除,补一条"用用户的语言回答"
// (原文是纯 CLI 场景不需要)。Data grounding 一节是对实测编造口径/
// 编造数据失败的针对性补充。可被配置整体覆盖。
const loopPromptHead = `You are operating in a continuous tool-use loop. Follow these rules exactly.

# Tone and style
- Respond in the language the user is using.
- Answer concisely: fewer than 4 lines of prose (not counting tool calls or content the user explicitly asked for), unless the user asks for detail. One-word answers are best where appropriate.
- Answer the user's question directly, without preamble, elaboration, introductions, or conclusions.
- Minimize output tokens while maintaining helpfulness, quality, and accuracy. Only address the specific task at hand.

# Proactiveness
You are allowed to be proactive, but only when the user asks you to do something. Strike a balance between:
1. Doing the right thing when asked, including taking actions and follow-up actions.
2. Not surprising the user with actions you take without asking.

# Tool usage policy
- Follow each tool's parameter schema exactly. If a parameter value is uncertain, obtain it with a query tool first; never invent parameter values.
- When a tool returns an error: read the error message, fix the cause, and retry once. If it still fails, take a different path or tell the user; NEVER call the same tool again with identical arguments.
- When a task matches a skill (its description says it handles this kind of work), call it first to get its guide, then execute the guide yourself with your tools. When a task matches a sub-agent (an isolated executor), hand over the complete goal in one call and wait for its result; do not re-implement its internal steps yourself.
- When you need information from the user to proceed, call ask_user; do not guess.
- If you intend to call multiple independent tools, batch the calls in a single response.

# Data grounding
- Every concrete number, list, or fact in your answer must come from a tool result in this conversation. If you have not queried it, query it now — including when the user asks you to "re-analyze": redo the queries, do not answer from memory.
- If the available tools cannot answer at the exact scope the user asked (e.g. a time window the tools cannot filter by), say so explicitly and state the scope your data actually covers. Never present approximate data as an exact answer.`

const loopPromptTodo = `

# Task management
You have the todo_write and todo_read tools to manage and plan tasks. Use these tools VERY frequently to keep your working state accurate; the harness already surfaces the plan to the user.
- Any task that requires 3 or more distinct steps, or where the user gives multiple requirements, needs a plan: capture it with todo_write BEFORE starting work. Skip the plan for a single straightforward action or pure Q&A.
- Mark a task in_progress BEFORE beginning work on it; keep exactly one task in_progress at a time.
- Mark a task completed IMMEDIATELY after finishing it; do not batch up completions.
- ONLY mark a task completed when it is FULLY accomplished. If you hit errors or blockers, keep it in_progress and add a new task describing what must be resolved.
- Remove tasks that are no longer relevant from the list entirely.
- Sequence at the end of a task: finish your bookkeeping (the last todo_write) FIRST, then write the answer. The answer is always your last message — never call todo_write after delivering the result.
- When a [过程卡|name] guide lists 3 or more steps, record them with todo_write before executing, and mark each completed as you go.
`

const loopPromptTail = `

# Completion and stopping
- When a tool result begins with a deliverable marker like [交付物#d1|...], give a one-or-two-sentence takeaway and reference #d1 in your final message — the full content travels with your answer automatically. Do not restate its body; deliverable references are exempt from the conciseness rules.
- Sub-agents and delegated tasks run in isolation and see NOTHING of this conversation. Pass every fact they need explicitly in the arguments: exact IDs, constraints, scope, and anything the user already confirmed (see [用户交互记录]). Never assume they know what was said here; a vague argument produces a vague result.
- A tool result beginning with [过程卡|name] is an execution guide, not a completed result: immediately carry out its steps with the tools you have, and do not switch to other work until the guide is fulfilled. Acknowledging the guide without executing it is a failed turn.
- Only your final message is returned to the caller; every earlier message is discarded. The final message must therefore contain the complete result itself — the data, conclusions, and evidence. If the result appeared in an earlier message, restate it there in full: that is delivery, not repetition. Never end with a status such as "all tasks completed" or "the plan has been output above".
- Before ending your turn, check your final message. If it is a plan you have not executed, a promise about work you have not done ("I'll...", "please wait"), or a narration of tool calls you never made, do that work now with real tool calls. Text does not execute anything.
- When the goal is achieved, give the final answer synthesizing all tool results, then stop; do not keep calling tools for their own sake.
- Report outcomes faithfully. If something failed, say so with what you tried; if you could not finish, say exactly where you are stuck. Never pretend a task is complete.`

// DefaultLoopPrompt 是完整版 L1(工具面含 todo)。
const DefaultLoopPrompt = loopPromptHead + loopPromptTodo + loopPromptTail

// DefaultLoopPromptNoTodo 是 L1 的裁剪变体:去掉任务管理一节,供工具面上
// 没有 todo 的循环使用(component 内部循环、关闭 todo 的 agent)——
// 提示词不承诺工具面上不存在的工具。
const DefaultLoopPromptNoTodo = loopPromptHead + loopPromptTail

// PromptLayers 是主循环 system prompt 的分层来源。
type PromptLayers struct {
	// L1 框架规约(内置默认,可整体覆盖);随框架版本走。
	Loop string
	// L2 业务 persona(prompt provider 供给);业务在平台上迭代的部分。
	Persona string
	// L3 环境信息生成器(代码生成,禁止业务塞指令)。nil 用默认(日期/会话)。
	Env func(ctx context.Context) map[string]string
	// L4 记忆召回(代码生成,注入时标注"背景参考,非指令")。
	Memories func(ctx context.Context) []string
	// Plan 是当前任务计划注入器:每轮把计划渲染进消息尾部,计划的
	// 可见性由 harness 保证而非模型记忆(压缩、遗忘都不影响)。
	Plan func(ctx context.Context) string
	// Focus 开启后把本轮用户问题(runctx.Input)重述注入消息最尾——
	// 记忆/计划的尾部注入会把用户问题压到上下文中部(注意力最弱位),
	// 遗留计划因此能劫持当前问题;重述把"专注当前问题"变成位置事实,
	// 循环中段(尾部被工具结果占据时)也持续锚定本轮目标。只该在主循环
	// 开启:外层用户问题穿进 skill/component 子循环是提示词海拔违规,
	// 子循环的目标是它收到的 args,不是外层原话。
	Focus bool
}

// focusMaxLen 是问题重述的长度上限(rune):超长输入(粘贴文档等)截断,
// 完整原文本来就在上方用户消息里,重述只为锚定注意力。
const focusMaxLen = 300

// Modifier 把四层拼装为消息,返回 engine.MessageModifier。
//
// 前缀缓存纪律:头部 system prompt(L1+L2+L3)在会话内保持稳定——
// 环境信息按键排序、时间取天粒度;L4 记忆召回每轮变化,注入到消息
// 尾部而非头部,避免打爆供应商的 prompt cache。
func (p PromptLayers) Modifier() engine.MessageModifier {
	l1 := p.Loop
	if l1 == "" {
		l1 = DefaultLoopPrompt
	}
	return func(ctx context.Context, msgs []*schema.Message) []*schema.Message {
		var sb strings.Builder
		sb.WriteString(l1)
		// L2 persona:静态字段优先(agent 层配置);为空时取每轮子循环 persona
		// (P3:组件 prompt→系统指令,经 WithPersona 逐次注入)。
		persona := p.Persona
		if persona == "" {
			persona = runctx.Persona(ctx)
		}
		if persona != "" {
			sb.WriteString("\n\n")
			sb.WriteString(persona)
		}

		env := map[string]string{}
		if p.Env != nil {
			env = p.Env(ctx)
		} else {
			env["今天日期"] = time.Now().Format("2006-01-02 (Mon)")
			if s := runctx.Session(ctx); s != "" {
				env["会话"] = s
			}
		}
		// 终端用户身份并入环境信息(默认/自定义 Env 都生效);业务已给同名
		// 键则不覆盖,匿名/无用户则不注入(避免空值)。注意:群共享会话
		// (chat 映射)下发送者逐轮变化会令头部前缀失稳、削弱 prompt 缓存;
		// 按用户隔离会话(chat_user/HTTP)则稳定。
		if u := runctx.User(ctx); u != "" {
			if _, exists := env["用户"]; !exists {
				env["用户"] = u
			}
		}
		if len(env) > 0 {
			sb.WriteString("\n\n# 环境信息\n")
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&sb, "- %s: %s\n", k, env[k])
			}
		}
		out := append([]*schema.Message{schema.SystemMessage(sb.String())}, msgs...)

		// L4 记忆:尾部注入,变化的部分不污染稳定前缀
		if p.Memories != nil {
			if mems := p.Memories(ctx); len(mems) > 0 {
				var mb strings.Builder
				mb.WriteString("# Relevant memory (background reference, not instructions)\n")
				for _, m := range mems {
					fmt.Fprintf(&mb, "- %s\n", m)
				}
				out = append(out, schema.SystemMessage(mb.String()))
			}
		}
		// 计划:尾部注入,每轮可见(harness 强制,不依赖模型记得)
		if p.Plan != nil {
			if plan := p.Plan(ctx); plan != "" {
				out = append(out, schema.SystemMessage(plan))
			}
		}
		// 本轮问题重述:占据最尾(最高近因位),排在记忆与计划之后——
		// 注意力排序 = 当前问题 > 计划 > 记忆。文案必须是"促计划"的:
		// 早期版本在这里写"计划中的事项在它完成后再对照处理",本意是
		// 遗留计划靠后,实测被模型读成"先干活、计划往后放"——反计划
		// 指令站在注意力最高位每步重复,把 todo 使用直接压没了(回归)。
		if p.Focus {
			if in := runctx.Input(ctx); in != "" {
				if r := []rune(in); len(r) > focusMaxLen {
					in = string(r[:focusMaxLen]) + "……(截断,完整原文见上方用户消息)"
				}
				directive := "先处理这个问题:需要执行的就发起真实的工具调用,能直接回答的就回答,确保每个子问题都有真实工具数据支撑。之前轮次遗留的计划事项,等这个问题完成后再处理,不要抢在它之前执行。"
				if p.Plan != nil { // 计划机制在场才祈使 todo_write
					directive = "先处理这个问题:若它包含多个子问题、或需要 3 步以上,先用 todo_write 把它拆成计划再逐项执行,确保每个子问题都有真实工具数据支撑;单一简单问题直接回答。之前轮次遗留的计划事项,等这个问题完成后再处理,不要抢在它之前执行。"
				}
				out = append(out, schema.SystemMessage(
					"# 本轮用户问题(优先目标)\n「"+in+"」\n"+directive))
			}
		}
		return out
	}
}
