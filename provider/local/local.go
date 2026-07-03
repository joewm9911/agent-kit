// Package local 把本地 Go 函数包装成能力,零配置成本。
package local

import (
	"context"

	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/cloverzhang/agent-kit/capability"
)

// Func 用泛型推断参数 schema,把一个 Go 函数变成能力(默认只读)。
// T 的字段通过 json/jsonschema tag 描述,和 eino utils.InferTool 一致:
//
//	type WeatherReq struct {
//	    City string `json:"city" jsonschema:"description=城市名"`
//	}
//	cap, _ := local.Func("get_weather", "查询城市天气", fn)
func Func[T, D any](name, description string, fn func(ctx context.Context, in T) (D, error)) (capability.Capability, error) {
	return FuncWithRisk(name, description, capability.RiskReadonly, fn)
}

// FuncWithRisk 同 Func,但显式声明风险级别(写操作应标记 mutating)。
func FuncWithRisk[T, D any](name, description string, risk capability.Risk, fn func(ctx context.Context, in T) (D, error)) (capability.Capability, error) {
	t, err := utils.InferTool(name, description, fn)
	if err != nil {
		return nil, err
	}
	ref := capability.Ref{Kind: "tool", Provider: "local", Namespace: "local", Name: name}
	return capability.FromTool(context.Background(), t, ref, risk)
}
