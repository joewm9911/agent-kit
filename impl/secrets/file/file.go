// Package file 提供凭证协议的文件实现:从一个不入库的 YAML 文件
// (name: value 平铺)取凭证。配置:{path: <文件路径>}。
package file

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/secrets"
)

func init() {
	secrets.RegisterProvider("file", func(conf map[string]any) (secrets.Provider, error) {
		path, _ := conf["path"].(string)
		return New(path)
	})
}

// Store 持有已加载的凭证表。
type Store struct {
	values map[string]string
}

// New 加载凭证文件。
func New(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	var values map[string]string
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse secrets file %s: %w", path, err)
	}
	return &Store{values: values}, nil
}

// Get 取一条凭证。
func (s *Store) Get(name string) (string, error) {
	v, ok := s.values[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found in secrets file", name)
	}
	return v, nil
}
