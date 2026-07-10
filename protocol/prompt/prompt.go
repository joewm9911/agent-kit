// Package prompt 把提示词抽象为 provider 供给的资源:按名字+版本通道
// 拉取,渲染变量,版本随轨迹打点可回溯。所有出现提示词的位置
// (system prompt、skill 模板、planner/replanner)都可以用
// cap://prompt.<type>/<source>/<name>@<label> 引用,或直接写字面量。
package prompt

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/core/capability"
)

// Template 是一个已解析的提示词模板。
type Template struct {
	Name    string
	Version string // 平台侧实际版本号,打点回溯用
	Text    string // 模板体,{var} 占位
}

var renderRef = regexp.MustCompile(`\{(\$?[\p{L}\p{N}_-]+)\}`)

// Render 渲染模板变量:单遍扫描,每个 {ident} 查一次表,未命中原样保留。
// 不做多轮替换——值里出现的 {占位} 字面量不会被二次展开(旧实现按 map
// 迭代序逐参数 ReplaceAll,值携带占位符时展开与否取决于随机迭代序)。
func (t *Template) Render(vars map[string]string) string {
	return renderRef.ReplaceAllStringFunc(t.Text, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
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
// nil 接收者安全:装配层常以可能为 nil 的 *Resolver 满足 Source 接口
// (typed-nil 穿过接口后非 nil),在此兜底成明确错误而非 panic。
func (r *Resolver) Resolve(ctx context.Context, refStr string) (*Template, error) {
	if r == nil {
		return nil, fmt.Errorf("prompt: ref %s used but no prompt sources configured", refStr)
	}
	ref, err := capability.ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	if ref.Kind != "prompt" {
		return nil, fmt.Errorf("prompt: ref %s is not a prompt ref", refStr)
	}
	r.mu.RLock()
	p, ok := r.providers[ref.Domain]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("prompt: unknown prompt source %q in ref %s", ref.Domain, refStr)
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

// RefPrefix 是提示词引用的识别前缀:以它开头的标量按引用解析
// (与 store/retriever 槽的"裸 type 或 cap://"同一识别模式)。
const RefPrefix = "cap://prompt"

// Value 是配置中的提示词字段:一律标量——cap://prompt 前缀 = 引用
// (装配期经提示词源解析锁版本),其余 = 字面量。
type Value struct {
	Literal string
	Ref     string
}

// UnmarshalYAML 只接受标量;旧的 {ref: ...} 映射写法直接报错并给出
// 新写法(fail fast 即迁移指南)。
func (v *Value) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf(`prompt value: 只接受标量——引用写 "cap://prompt/<source>/<name>",字面量直接写文本({ref: ...} 映射写法已移除)`)
	}
	if strings.HasPrefix(s, RefPrefix) {
		v.Ref = s
	} else {
		v.Literal = s
	}
	return nil
}

// IsZero 报告该字段是否未配置。
func (v Value) IsZero() bool { return v.Literal == "" && v.Ref == "" }

// Source 是引用解析的最小契约:消费方(skill/loop/config 装配)持它而非
// 具体 *Resolver,测试可注入假源。*Resolver 天然实现。
type Source interface {
	Resolve(ctx context.Context, refStr string) (*Template, error)
}

// Resolve 解析该字段:字面量直接返回(version 记为 inline),
// 引用则经 resolver 拉取。
func (v Value) Resolve(ctx context.Context, r Source) (*Template, error) {
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
