// Package registry 提供模型工厂注册表与配置解码工具。
// (能力供给的注册表见 source 包,提示词见 prompt 包,通道见 channel 包。)
package registry

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/eino/components/model"
)

// ModelFactory 按配置构造 ChatModel。
type ModelFactory func(ctx context.Context, conf map[string]any) (model.ToolCallingChatModel, error)

var (
	mu           sync.RWMutex
	mdlFactories = map[string]ModelFactory{}
)

// RegisterModel 注册模型工厂,重复注册会 panic(视为编程错误)。
func RegisterModel(provider string, f ModelFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := mdlFactories[provider]; ok {
		panic(fmt.Sprintf("registry: model factory %q already registered", provider))
	}
	mdlFactories[provider] = f
}

// BuildModel 按 provider 实例化模型。
func BuildModel(ctx context.Context, provider string, conf map[string]any) (model.ToolCallingChatModel, error) {
	mu.RLock()
	f, ok := mdlFactories[provider]
	mu.RUnlock()
	if !ok {
		names := make([]string, 0, len(mdlFactories))
		for k := range mdlFactories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("registry: unknown model provider %q, registered: %v", provider, names)
	}
	return f(ctx, conf)
}
