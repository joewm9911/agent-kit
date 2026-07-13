// Package askuser 提供内置的 ask_user 能力:大脑主动向用户求澄清。
package askuser

import (
	"context"
	"errors"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// New 返回 ask_user 能力。实际的提问通道来自 runctx(CLI 阻塞读、
// 飞书发消息等回复),无通道时以工具结果告知模型,让它自行决定如何继续。
func New() capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "ask_user"},
		Description: "Ask the user one question and wait for the answer. Use only when required information is missing and cannot be obtained through other tools; ask only one question at a time.",
		Params:      capability.SingleParam("question", "The question to ask the user, concise and specific"),
		Tags:        []string{capability.TagInteractive}, // 等人回复不占工具超时
		Risk:        capability.RiskReadonly,             // 只向用户提问,不动外部世界
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		question := capability.ParseSingle(argsJSON, "question")
		it := runctx.GetInteractor(ctx)
		if it == nil {
			return "当前运行环境没有用户交互通道,无法提问。请基于已有信息继续,或在最终回答中列出需要用户补充的内容。", nil
		}
		answer, err := it.Ask(ctx, question)
		if err != nil {
			// 挂起不是失败:必须原样上抛让整轮退栈,等答案到达后重放。
			var suspended *suspend.ErrSuspended
			if errors.As(err, &suspended) {
				return "", err
			}
			return "提问失败:" + err.Error() + "。请基于已有信息继续。", nil
		}
		// 问答是真实用户对话:记入轮级交互日志,由 agent 收口落会话——
		// 子循环内的问答不再随子循环丢弃(下一轮大脑可见,不重问)。
		runctx.RecordInteraction(ctx, question, answer)
		return "用户回答:" + answer, nil
	})
}
