package builtin

import (
	"context"
	"errors"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
	"github.com/joewm9911/agent-kit/suspend"
)

// AskUser 返回 ask_user 能力:大脑主动向用户求澄清。
// 实际的提问通道来自 runctx(CLI 阻塞读、飞书发消息等回复),
// 无通道时以工具结果告知模型,让它自行决定如何继续。
func AskUser() capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "builtin", Name: "ask_user"},
		Description: "向用户提一个问题并等待回答。仅在缺少必要信息、且无法通过其他工具获取时使用;一次只问一个问题。",
		Params:      capability.SingleParam("question", "要问用户的问题,简洁明确"),
		Tags:        []string{capability.TagInteractive}, // 等人回复不占工具超时
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
		return "用户回答:" + answer, nil
	})
}
