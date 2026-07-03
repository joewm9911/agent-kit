package capability

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// NewModel 把 ChatModel 纳入能力体系:AsTool 时是可被上级大脑调用的
// "问答子模型"(如廉价小模型做摘要),AsLambda 时是图里的模型节点。
func NewModel(ref Ref, description string, m model.ToolCallingChatModel) Capability {
	ref.Kind = "model"
	meta := Meta{
		Ref:         ref,
		Description: description,
		Params:      SingleParam("prompt", "发给该模型的完整提示词"),
	}
	return New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		prompt := ParseSingle(argsJSON, "prompt")
		out, err := m.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
		if err != nil {
			return "", err
		}
		return out.Content, nil
	})
}
