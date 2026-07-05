// Package rpctool 把 RPC 接口暴露为能力。
//
// RPC 框架各异(kitex 泛化调用、gRPC 反射、thrift 等),本包不绑定任何
// 一种,只定义 Invoker 契约:接入方实现"service/method + JSON 请求 →
// JSON 响应"的泛化调用,用 source.Static 打包成源即可进目录:
//
//	caps := []capability.Capability{
//	    rpctool.New("order", rpctool.Method{...}, myKitexInvoker),
//	}
//	catalog.AddSource(ctx, source.Static("order-rpc", caps...), true, 0)
//
// kitex:genericclient + generic.JSONThriftGeneric 天然满足该契约;
// gRPC:reflection + protojson 即可实现。
package rpctool

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

// Invoker 是 RPC 泛化调用的最小契约。
type Invoker interface {
	Invoke(ctx context.Context, service, method, reqJSON string) (respJSON string, err error)
}

// Method 声明一个要暴露给模型的 RPC 方法。
type Method struct {
	Service     string
	Method      string
	Name        string // 工具名,默认 service_method
	Description string
	Risk        capability.Risk
	// Params 描述请求体字段;为 nil 时模型直接产出整个请求 JSON。
	Params map[string]*schema.ParameterInfo
}

// New 把一个 RPC 方法包装成能力,namespace 为所属 source 名。
func New(namespace string, m Method, inv Invoker) capability.Capability {
	name := m.Name
	if name == "" {
		name = m.Service + "_" + m.Method
	}
	params := m.Params
	if params == nil {
		params = map[string]*schema.ParameterInfo{
			"request": {Type: schema.Object, Desc: "请求体 JSON 对象", Required: true},
		}
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: namespace, Name: name},
		Description: m.Description,
		Params:      schema.NewParamsOneOfByParams(params),
		Risk:        m.Risk,
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		resp, err := inv.Invoke(ctx, m.Service, m.Method, argsJSON)
		if err != nil {
			// RPC 错误同样回传给模型,让它决定下一步。
			return fmt.Sprintf("rpc error: %v", err), nil
		}
		return resp, nil
	})
}
