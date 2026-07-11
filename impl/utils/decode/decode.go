// Package decode 是协议实现方的配置解码工具:把 map 形式的配置解码到
// 各实现自己的强类型配置结构上(JSON round-trip,足够覆盖 YAML 基础类型)。
package decode

import (
	"bytes"
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

// StrictConfig 同 Config,但未知键报错。配置面固定(没有自由扩展段)
// 的实现应当用它:拼错的键静默忽略等于配置没生效还不吱声。
func StrictConfig(conf map[string]any, target any) error {
	b, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}
