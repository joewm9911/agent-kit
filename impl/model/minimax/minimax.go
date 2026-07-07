package minimax

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/model"
)

// minimax 走 MiniMax 的 OpenAI 兼容接口(已验证 tool calling 与 usage 回报)。
//
//	provider: minimax
//	config:
//	  api_key: ${MINIMAX_API_KEY}
//	  model: MiniMax-M2.7           # 可省略,默认 M2.7
//	  # base_url: https://api.minimax.io/v1   # 海外平台的 key 用这个
//
// 注意:M 系是推理模型,content 内联 <think> 块——引擎侧 JSON 解析已
// 适配(engine.ExtractJSON 剥 think 块);旧款 MiniMax-Text-01 无 think
// 形态但能力较弱(真机对照见 docs/prompt-inventory.md P4 节)。
func init() {
	model.Register("minimax", func(ctx context.Context, conf map[string]any) (einomodel.ToolCallingChatModel, error) {
		var cfg struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		}
		if err := decode.Config(conf, &cfg); err != nil {
			return nil, err
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.minimaxi.com/v1" // 国内平台
		}
		if cfg.Model == "" {
			cfg.Model = "MiniMax-M2.7"
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
		})
	})
}
