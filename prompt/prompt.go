// Package prompt 把提示词抽象为 provider 供给的资源:按名字+版本通道
// 拉取,渲染变量,版本随轨迹打点可回溯。所有出现提示词的位置
// (system prompt、skill 模板、planner/replanner)都可以用
// cap://prompt.<type>/<source>/<name>@<label> 引用,或直接写字面量。
package prompt

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/capability"
)

// Template 是一个已解析的提示词模板。
type Template struct {
	Name    string
	Version string // 平台侧实际版本号,打点回溯用
	Text    string // 模板体,{var} 占位
}

// Render 渲染模板变量。
func (t *Template) Render(vars map[string]string) string {
	out := t.Text
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

// Provider 是提示词供给端的最小契约。label 是版本通道
// (production/staging/具体版本号),空串取默认。
type Provider interface {
	Get(ctx context.Context, name, label string) (*Template, error)
}

// Factory 按配置构造 Provider。
type Factory func(conf map[string]any) (Provider, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册 provider 类型(inline/file/http/自定义平台)。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("prompt: provider type %q already registered", typ))
	}
	factories[typ] = f
}

// NewProvider 按类型实例化 provider。
func NewProvider(typ string, conf map[string]any) (Provider, error) {
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("prompt: unknown provider type %q", typ)
	}
	return f(conf)
}

// Resolver 聚合多个命名的 provider,按 CapRef 解析提示词引用。
// namespace 段即 provider 实例名,天然隔离多个提示词平台。
type Resolver struct {
	mu           sync.RWMutex
	providers    map[string]Provider // name -> provider
	defaultLabel string
}

// NewResolver 创建解析器,defaultLabel 是 ref 未带 @label 时的默认版本通道。
func NewResolver(defaultLabel string) *Resolver {
	return &Resolver{providers: map[string]Provider{}, defaultLabel: defaultLabel}
}

// Add 挂载一个命名 provider。
func (r *Resolver) Add(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
}

// Resolve 解析 cap://prompt.*/<source>/<name>@<label> 形式的引用。
func (r *Resolver) Resolve(ctx context.Context, refStr string) (*Template, error) {
	ref, err := capability.ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	if ref.Kind != "prompt" {
		return nil, fmt.Errorf("prompt: ref %s is not a prompt ref", refStr)
	}
	r.mu.RLock()
	p, ok := r.providers[ref.Namespace]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("prompt: unknown prompt source %q in ref %s", ref.Namespace, refStr)
	}
	label := ref.Version
	if label == "" {
		label = r.defaultLabel
	}
	t, err := p.Get(ctx, ref.Name, label)
	if err != nil {
		return nil, fmt.Errorf("prompt: get %s: %w", refStr, err)
	}
	return t, nil
}

// Value 是配置中的提示词字段:字面量字符串或 {ref: cap://...}。
// 实现 yaml 自定义解析,两种写法都兼容。
type Value struct {
	Literal string
	Ref     string
}

// UnmarshalYAML 支持标量(字面量)与 {ref: ...} 两种写法。
func (v *Value) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		v.Literal = s
		return nil
	}
	var m struct {
		Ref string `yaml:"ref"`
	}
	if err := unmarshal(&m); err != nil {
		return err
	}
	if m.Ref == "" {
		return fmt.Errorf("prompt value: expect string literal or {ref: cap://prompt...}")
	}
	v.Ref = m.Ref
	return nil
}

// IsZero 报告该字段是否未配置。
func (v Value) IsZero() bool { return v.Literal == "" && v.Ref == "" }

// Resolve 解析该字段:字面量直接返回(version 记为 inline),
// 引用则经 resolver 拉取。
func (v Value) Resolve(ctx context.Context, r *Resolver) (*Template, error) {
	if v.Literal != "" {
		return &Template{Name: "inline", Version: "inline", Text: v.Literal}, nil
	}
	if v.Ref == "" {
		return &Template{}, nil
	}
	if r == nil {
		return nil, fmt.Errorf("prompt: ref %s used but no prompt sources configured", v.Ref)
	}
	return r.Resolve(ctx, v.Ref)
}
