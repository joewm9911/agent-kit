// Package openai 注册 openai 兼容模型工厂。其他厂商(ark、claude、qwen 等)
// 参照本文件用 eino-ext 对应组件注册即可。
package openai

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/model"
)

func init() {
	// provider: openai —— 兼容所有 OpenAI 协议的服务(含各家代理网关)。
	model.Register("openai", func(ctx context.Context, conf map[string]any) (einomodel.ToolCallingChatModel, error) {
		var cfg struct {
			APIKey      string   `json:"api_key"`
			BaseURL     string   `json:"base_url"`
			Model       string   `json:"model"`
			Temperature *float32 `json:"temperature"`
			TopP        *float32 `json:"top_p"`
			MaxTokens   *int     `json:"max_tokens"`
		}
		// 严格解码:采样参数此前静默丢弃(temperature 配了不生效),
		// 未知键直接报错。
		if err := decode.StrictConfig(conf, &cfg); err != nil {
			return nil, err
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:      cfg.APIKey,
			BaseURL:     cfg.BaseURL,
			Model:       cfg.Model,
			Temperature: cfg.Temperature, TopP: cfg.TopP, MaxTokens: cfg.MaxTokens,
		})
	})
}
