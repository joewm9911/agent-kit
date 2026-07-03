package loop

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

// DefaultMaxToolResultLen 是工具结果进入上下文的默认截断长度(rune)。
// MCP 等外部工具可能返回任意大的结果,不设防会直接打爆窗口。
const DefaultMaxToolResultLen = 8000

// TruncateResults 给能力集套上结果截断闸门(Ring 0):任何工具的
// 返回超过 maxLen 即截断并附说明,让模型知道内容不完整、可换参数
// 缩小范围。maxLen==0 用默认值,<0 关闭截断。
func TruncateResults(caps []capability.Capability, maxLen int) []capability.Capability {
	if maxLen < 0 {
		return caps
	}
	if maxLen == 0 {
		maxLen = DefaultMaxToolResultLen
	}
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &truncated{inner: c, maxLen: maxLen})
	}
	return out
}

type truncated struct {
	inner  capability.Capability
	maxLen int
}

func (t *truncated) Meta() capability.Meta { return t.inner.Meta() }

func (t *truncated) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := t.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", t.inner.Meta().Ref)
	}
	return &truncatedTool{inner: inv, maxLen: t.maxLen}, nil
}

func (t *truncated) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		out, err := capability.Invoke(ctx, t.inner, argsJSON)
		if err != nil {
			return "", err
		}
		return clip(out, t.maxLen), nil
	}), nil
}

type truncatedTool struct {
	inner  tool.InvokableTool
	maxLen int
}

func (t *truncatedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx) // 短名与描述不变,截断对模型透明
}

func (t *truncatedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		return "", err
	}
	return clip(out, t.maxLen), nil
}

func clip(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) +
		fmt.Sprintf("\n...[结果过长,已截断:共 %d 字符,仅保留前 %d。如需完整内容请缩小查询范围]", len(r), maxLen)
}
