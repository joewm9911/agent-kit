// Package models 注册内置模型工厂。其他厂商(ark、claude、qwen 等)
// 参照本文件用 eino-ext 对应组件注册即可。
package openai

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/registry"
)

func init() {
	// provider: openai —— 兼容所有 OpenAI 协议的服务(含各家代理网关)。
	registry.RegisterModel("openai", func(ctx context.Context, conf map[string]any) (model.ToolCallingChatModel, error) {
		var cfg struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		}
		if err := registry.DecodeConfig(conf, &cfg); err != nil {
			return nil, err
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
		})
	})
}
