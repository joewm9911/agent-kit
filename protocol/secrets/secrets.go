// Package secrets 是凭证协议:配置文本中的 ${ENV_VAR} 与 ${secret:NAME}
// 占位符在加载时展开,凭证不落配置文件。Provider 按 type 注册(file/vault/
// 自定义在 impl/secrets/*);env 是零依赖默认,随协议包常驻(比照
// store.InMemory 的例外:读环境变量不应要求空导入)。
package secrets

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Provider 是凭证来源的最小契约。
type Provider interface {
	Get(name string) (string, error)
}

// Factory 按配置构造一个 Provider。
type Factory func(conf map[string]any) (Provider, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// RegisterProvider 注册凭证来源类型,重复注册会 panic(视为编程错误)。
func RegisterProvider(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("secrets: provider type %q already registered", typ))
	}
	factories[typ] = f
}

// New 按类型构造 Provider,空类型默认 env。未注册的类型 fail fast。
func New(typ string, conf map[string]any) (Provider, error) {
	if typ == "" {
		typ = "env"
	}
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		names := make([]string, 0, len(factories))
		for k := range factories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("secrets: unknown provider type %q, registered: %v", typ, names)
	}
	return f(conf)
}

// Env 从环境变量取凭证(零依赖默认)。
type Env struct{}

func (Env) Get(name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secret %q not found in environment", name)
	}
	return v, nil
}

func init() {
	RegisterProvider("env", func(_ map[string]any) (Provider, error) { return Env{}, nil })
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
