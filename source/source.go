// Package source 定义能力供给的三层中第一层:Source(供给源)。
// MCP server、HTTP 接口声明、A2A 远端、本地代码,都是"能列举自己
// 有什么能力"的端点。多个 source 聚合进 Catalog,agent 只面向 Catalog。
package source

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloverzhang/agent-kit/capability"
)

// Source 是能力供给端的最小契约。
type Source interface {
	// Name 即该源下所有能力的 namespace。
	Name() string
	// Sync 连接供给端并返回全部能力。实现应保证可重复调用(刷新)。
	Sync(ctx context.Context) ([]capability.Capability, error)
}

// Factory 按配置构造 Source。name 是配置里声明的源名(namespace)。
type Factory func(ctx context.Context, name string, conf map[string]any) (Source, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册 source 类型(mcp/http/rpc/a2a/自定义)。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("source: type %q already registered", typ))
	}
	factories[typ] = f
}

// New 按类型实例化 Source。
func New(ctx context.Context, typ, name string, conf map[string]any) (Source, error) {
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("source: unknown type %q", typ)
	}
	return f(ctx, name, conf)
}

// Static 把代码侧构造好的能力包装成一个 Source,
// 本地 Go 函数、子 agent 由此进入目录。
func Static(name string, caps ...capability.Capability) Source {
	return &static{name: name, caps: caps}
}

type static struct {
	name string
	caps []capability.Capability
}

func (s *static) Name() string { return s.name }

func (s *static) Sync(_ context.Context) ([]capability.Capability, error) {
	return s.caps, nil
}
