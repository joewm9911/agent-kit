package models

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/cloverzhang/agent-kit/registry"
)

// minimax 走 MiniMax 的 OpenAI 兼容接口(已验证 tool calling 与 usage 回报)。
//
//	provider: minimax
//	config:
//	  api_key: ${MINIMAX_API_KEY}
//	  model: MiniMax-Text-01        # 可省略;主循环推荐 Text-01
//	  # base_url: https://api.minimax.io/v1   # 海外平台的 key 用这个
//
// 注意:MiniMax-M1 是推理模型,输出带 <think> 块,适合作为 skill 的
// 专属模型跑重推理任务;主循环(ReAct)默认用 MiniMax-Text-01。
func init() {
	registry.RegisterModel("minimax", func(ctx context.Context, conf map[string]any) (model.ToolCallingChatModel, error) {
		var cfg struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		}
		if err := registry.DecodeConfig(conf, &cfg); err != nil {
			return nil, err
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.minimaxi.com/v1" // 国内平台
		}
		if cfg.Model == "" {
			cfg.Model = "MiniMax-Text-01"
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
		})
	})
}
