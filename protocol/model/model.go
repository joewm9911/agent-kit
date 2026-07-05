// Package model 是模型协议:按 provider 注册工厂、按配置构造 ChatModel。
// 与 session/memory/prompt/source/channel 的注册表机制同构;官方实现见
// impl/model/*(minimax/openai),空导入即注册。
package model

import (
	"context"
	"fmt"
	"sort"
	"sync"

	einomodel "github.com/cloudwego/eino/components/model"
)

// Factory 按配置构造 ChatModel。
type Factory func(ctx context.Context, conf map[string]any) (einomodel.ToolCallingChatModel, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册模型工厂,重复注册会 panic(视为编程错误)。
func Register(provider string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factories[provider]; ok {
		panic(fmt.Sprintf("model: factory %q already registered", provider))
	}
	factories[provider] = f
}

// Build 按 provider 实例化模型。
func Build(ctx context.Context, provider string, conf map[string]any) (einomodel.ToolCallingChatModel, error) {
	mu.RLock()
	f, ok := factories[provider]
	mu.RUnlock()
	if !ok {
		names := make([]string, 0, len(factories))
		for k := range factories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("model: unknown provider %q, registered: %v", provider, names)
	}
	return f(ctx, conf)
}
