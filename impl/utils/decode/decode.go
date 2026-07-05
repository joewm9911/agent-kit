// Package decode 是协议实现方的配置解码工具:把 map 形式的配置解码到
// 各实现自己的强类型配置结构上(JSON round-trip,足够覆盖 YAML 基础类型)。
package decode

import (
	"encoding/json"
	"fmt"
)

// Config 把 conf 解码到 target。
func Config(conf map[string]any, target any) error {
	b, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}
