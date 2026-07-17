package config

// live 测试共享夹具:真机 provider 表 + 计数假工具(机制断言用)。

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/loop"

	_ "github.com/joewm9911/agent-kit/impl/model/zhipu"
)

// engineLiveProviders 返回在场的真机 provider(key 环境变量决定)。
func engineLiveProviders(t *testing.T) map[string]func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
	t.Helper()
	retry := loop.RetryConfig{MaxAttempts: 4, BaseDelay: loop.Duration(3 * time.Second), MaxDelay: loop.Duration(30 * time.Second)}
	out := map[string]func(ctx context.Context) (einomodel.ToolCallingChatModel, error){}
	if key := os.Getenv("ZHIPU_API_KEY"); key != "" {
		out["zhipu/glm-5.2"] = func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
			m, err := model.Build(ctx, "zhipu", map[string]any{"api_key": key})
			if err != nil {
				return nil, err
			}
			return loop.RetryModel(m, retry), nil
		}
	}
	if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
		base := os.Getenv("SMOKE_MODEL_BASE")
		if base == "" {
			base = "https://api.minimaxi.com/v1"
		}
		name := os.Getenv("SMOKE_MINIMAX_MODEL") // 型号对照用;空 = 厂商默认 M2.7
		if name == "" {
			name = "MiniMax-M2.7"
		}
		out["minimax/"+name] = func(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
			m, err := model.Build(ctx, "minimax", map[string]any{"api_key": key, "base_url": base, "model": name})
			if err != nil {
				return nil, err
			}
			return loop.RetryModel(m, retry), nil
		}
	}
	return out
}

// liveTool 构造带调用计数与末次参数记录的假工具(机制断言用)。
func liveTool(name, desc string, params *schema.ParamsOneOf, fn func(argsJSON string) string) (capability.Capability, *atomic.Int32, *atomic.Value) {
	var calls atomic.Int32
	var lastArgs atomic.Value
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "live", Name: name},
		Description: desc,
		Params:      params,
		Risk:        capability.RiskReadonly, // 未声明按 mutating 保守拦审批,测试工具需显式只读
	}
	c := capability.New(meta, func(_ context.Context, argsJSON string) (string, error) {
		calls.Add(1)
		lastArgs.Store(argsJSON)
		return fn(argsJSON), nil
	})
	return c, &calls, &lastArgs
}

func strParam(name, desc string) *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		name: {Type: schema.String, Desc: desc, Required: true},
	})
}
