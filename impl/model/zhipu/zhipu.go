// Package zhipu 走智谱 GLM 的 OpenAI 兼容接口(bigmodel.cn)。
//
//	provider: zhipu
//	config:
//	  api_key: ${ZHIPU_API_KEY}
//	  model: glm-5.2              # 可省略,默认 glm-5.2
//	  # base_url: https://open.bigmodel.cn/api/paas/v4
package zhipu

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/model"
)

func init() {
	model.Register("zhipu", func(ctx context.Context, conf map[string]any) (einomodel.ToolCallingChatModel, error) {
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
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://open.bigmodel.cn/api/paas/v4"
		}
		if cfg.Model == "" {
			cfg.Model = "glm-5.2"
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:      cfg.APIKey,
			BaseURL:     cfg.BaseURL,
			Model:       cfg.Model,
			Temperature: cfg.Temperature, TopP: cfg.TopP, MaxTokens: cfg.MaxTokens,
		})
	})
}
