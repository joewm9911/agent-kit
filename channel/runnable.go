package channel

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// Runnable 是分发层/服务层对 agent 的最小契约(消费方定义):
// 运行一轮、流式一轮、身份描述、运行控制。*agent.Agent 天然实现;
// 服务层只依赖此接口,测试与装饰(代理/限流/审计包装)无需真 Agent。
type Runnable interface {
	Name() string
	Description() string
	Run(ctx context.Context, sessionID, input string) (string, error)
	Stream(ctx context.Context, sessionID, input string) (*schema.StreamReader[*schema.Message], error)
	// Interrupt 叫停会话当前运行;Steer 向运行中注入插话。均幂等,无运行时为空操作。
	Interrupt(sessionID string)
	Steer(sessionID, msg string)
}
