package skill

import (
	"context"

	"github.com/joewm9911/agent-kit/core/capability"
)

// testCap 构造一个记录输入并返回 fn(输入) 的能力。
func testCap(name string, fn func(ctx context.Context, args string) (string, error)) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: name},
	}, fn)
}
