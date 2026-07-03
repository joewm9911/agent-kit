// Package secrets 提供凭证管理:配置文本中的 ${ENV_VAR} 与
// ${secret:NAME} 占位符在加载时展开,凭证不落配置文件。
// Provider 可替换为 vault 等实现,接口不变。
package secrets

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider 是凭证来源的最小契约。
type Provider interface {
	Get(name string) (string, error)
}

// Env 从环境变量取凭证。
type Env struct{}

func (Env) Get(name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secret %q not found in environment", name)
	}
	return v, nil
}

// File 从一个不入库的 YAML 文件(name: value 平铺)取凭证。
type File struct {
	values map[string]string
}

// NewFile 加载凭证文件。
func NewFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	var values map[string]string
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse secrets file %s: %w", path, err)
	}
	return &File{values: values}, nil
}

func (f *File) Get(name string) (string, error) {
	v, ok := f.values[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found in secrets file", name)
	}
	return v, nil
}

var placeholder = regexp.MustCompile(`\$\{(secret:)?(\w+)\}`)

// Expand 展开配置文本中的占位符:${NAME} 走环境变量,
// ${secret:NAME} 走 provider。任一解析失败即报错(fail fast),
// 避免空凭证静默传下去。注释行(# 开头)不做展开,
// 注释掉的配置块不应导致加载失败。
func Expand(data []byte, p Provider) ([]byte, error) {
	var firstErr error
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines[i] = placeholder.ReplaceAllStringFunc(line, func(m string) string {
			groups := placeholder.FindStringSubmatch(m)
			name := groups[2]
			if groups[1] != "" { // ${secret:NAME}
				if p == nil {
					firstErr = fmt.Errorf("secret %q referenced but no secrets provider configured", name)
					return m
				}
				v, err := p.Get(name)
				if err != nil && firstErr == nil {
					firstErr = err
				}
				return v
			}
			v, ok := os.LookupEnv(name)
			if !ok && firstErr == nil {
				firstErr = fmt.Errorf("environment variable %q not set", name)
			}
			return v
		})
	}
	return []byte(strings.Join(lines, "\n")), firstErr
}

// Redact 把文本中出现的凭证值替换为 ***,日志脱敏用。
func Redact(text string, values ...string) string {
	for _, v := range values {
		if len(v) >= 6 {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return text
}
