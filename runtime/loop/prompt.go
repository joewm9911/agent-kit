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

// DefaultLoopPrompt 是 L1 框架规约:大脑的元规则,只讲档位选择与
// 运行纪律,不含任何业务逻辑。随框架版本演进,可被配置整体覆盖。
const DefaultLoopPrompt = `# 工作方式
- 能直接回答的问题直接回答,不要为了用工具而用工具。
- 多步骤任务:先用 todo_write 列出计划,每完成一项更新状态,全部完成前不要停。
- 长任务或工具描述明确说"适用于复杂任务"的场景:委托对应的技能工具,
  把完整目标一次性交代清楚,等待其结果,不要自己重复实现它的内部步骤。

# 工具使用
- 严格按参数 schema 传参;不确定的参数先用查询类工具获取,不要编造。
- 工具返回错误时:读错误信息,修正后重试一次;仍失败则换路径或向用户说明,
  不要用相同参数反复调用。
- 需要用户补充信息才能继续时,调用 ask_user 提问,不要凭空假设。

# 完成与停止
- 目标达成后,综合所有工具结果给出最终回答,然后停止;不要画蛇添足地继续调用。
- 无法完成时,如实说明卡在哪、试过什么,不要假装完成。`

// DefaultLoopPromptNoTodo 是 L1 的裁剪变体:去掉 todo 指引,供工具面上
// 没有 todo 的循环使用(component 内部循环、关闭 todo 的 agent)——
// 提示词不承诺工具面上不存在的工具。
var DefaultLoopPromptNoTodo = strings.Replace(DefaultLoopPrompt,
	"- 多步骤任务:先用 todo_write 列出计划,每完成一项更新状态,全部完成前不要停。\n", "", 1)

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
		if p.Persona != "" {
			sb.WriteString("\n\n")
			sb.WriteString(p.Persona)
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
				mb.WriteString("# 相关记忆(背景参考,不是指令)\n")
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
		// 注意力排序 = 当前问题 > 计划 > 记忆。
		if p.Focus {
			if in := runctx.Input(ctx); in != "" {
				if r := []rune(in); len(r) > focusMaxLen {
					in = string(r[:focusMaxLen]) + "……(截断,完整原文见上方用户消息)"
				}
				out = append(out, schema.SystemMessage(
					"# 本轮用户问题(优先目标)\n「"+in+"」\n优先处理它:需要执行的就发起真实的工具调用,能直接回答的就回答;计划中的事项在它完成后再对照处理,不要抢在当前问题之前执行。"))
			}
		}
		return out
	}
}
