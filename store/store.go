// Package store 提供 KV 家族存储原语。todo 计划清单、digest 大结果暂存
// 等内部状态在其上做薄适配(见 builtin/todo.go、loop/digest.go)。
//
// 设计要点是原子性与生命周期焊进原语:
//   - Update 是原子读改写(RMW),后端保证并发串行化。todo 的 list+stale
//     读改写、nudge 自增都靠它,分布式多副本下不丢更新;
//   - TTL 是 Update/写入的入参,支持的后端(redis)honor,inmemory 惰性过期。
//
// 后端按 type 注册,与 session/prompt/source 的工厂机制同构:实现方空导入
// 即可在模块块里以 store: <type> 引用。
package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// KV 是 KV 家族存储的最小契约。
type KV interface {
	// Get 返回键值与是否存在。返回的切片是副本,调用方可安全持有。
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Update 原子读改写:mutate 收到旧值(ok 指示是否存在)并返回新值,
	// 后端保证读-改-写整体串行化。mutate 返回 nil 新值表示删除该键。
	// ttl>0 时新值带过期,<=0 不过期。
	Update(ctx context.Context, key string, mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error
	// Delete 删除键,不存在也不报错。
	Delete(ctx context.Context, key string) error
	// Scan 返回所有以 prefix 开头的键(用于按 scope 清理)。
	Scan(ctx context.Context, prefix string) ([]string, error)
}

// BackendFactory 按配置构造一个 KV 后端。
type BackendFactory func(conf map[string]any) (KV, error)

var (
	facMu     sync.RWMutex
	factories = map[string]BackendFactory{}
)

// RegisterBackend 注册 KV 后端类型(inmemory/redis/自定义)。
func RegisterBackend(typ string, f BackendFactory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("store: backend type %q already registered", typ))
	}
	factories[typ] = f
}

// NewBackend 按类型构造后端,空类型默认 inmemory。
func NewBackend(typ string, conf map[string]any) (KV, error) {
	if typ == "" {
		typ = "inmemory"
	}
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("store: unknown backend type %q", typ)
	}
	return f(conf)
}

func init() {
	RegisterBackend("inmemory", func(_ map[string]any) (KV, error) {
		return NewInMemory(), nil
	})
}
