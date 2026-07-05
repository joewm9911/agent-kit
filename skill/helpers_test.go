package skill

import (
	"context"
	"fmt"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
)

// testCap 构造一个记录输入并返回 fn(输入) 的能力。
func testCap(name string, fn func(ctx context.Context, args string) (string, error)) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: name},
	}, fn)
}

// resolverFor 按名字查表解析步骤引用(编排图测试用)。
func resolverFor(caps map[string]capability.Capability) engine.StepResolver {
	return func(use string) (capability.Capability, error) {
		if c, ok := caps[use]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("unknown use %q", use)
	}
}
