package registry

import (
	"encoding/json"
	"fmt"
)

// DecodeConfig 把 map 形式的配置解码到 provider 自己的强类型配置结构上
// (JSON round-trip,足够覆盖 YAML 解析出的基础类型)。
func DecodeConfig(conf map[string]any, target any) error {
	b, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}
