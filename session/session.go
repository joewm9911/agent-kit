// Package session 定义会话历史存储的契约与工厂,不含具体后端实现。
// 历史织入由 agent 运行时自动完成(模型无感知)——该不该带历史不是模型
// 的决策,是流程保证。后端(inmemory/file/redis/…)在 impl/session/* 下实现
// 并 init 自注册,消费方只经 New 工厂拿 Store,不直接构造具体后端。
package session

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Store 是会话历史存储的最小契约。Load 返回窗口裁剪后的最近消息。
type Store interface {
	Load(ctx context.Context, sessionID string) ([]*schema.Message, error)
	Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error
	Clear(ctx context.Context, sessionID string) error
}

// FullLoader 是可选扩展:返回不裁剪的全量历史。实现它的后端可享受
// 滚动摘要持久化与会话内相关性召回;内置 inmemory/file 均已实现。
type FullLoader interface {
	LoadAll(ctx context.Context, sessionID string) ([]*schema.Message, error)
}

// Factory 按配置构造存储。window 为保留的最近消息条数,<=0 不裁剪
// (裁剪只影响 Load 结果,持久化后端应保留全量记录)。
type Factory func(conf map[string]any, window int) (Store, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册存储类型(inmemory/file/redis/自定义):实现方 init 自注册,
// 空导入(或经 agent-kit/std)即可在配置里以 store: <type> 引用。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("session: store type %q already registered", typ))
	}
	factories[typ] = f
}

// New 按类型构造存储,空类型默认 inmemory。后端未注册时 fail-fast(提示
// 空导入 impl 后端或 agent-kit/std)。
func New(typ string, conf map[string]any, window int) (Store, error) {
	if typ == "" {
		typ = "inmemory"
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
		return nil, fmt.Errorf("session: unknown store type %q; blank-import a backend (e.g. agent-kit/impl/session/inmemory) or agent-kit/std. registered: %v", typ, names)
	}
	return f(conf, window)
}

// Trim 按窗口裁剪消息(window<=0 不裁剪),供各 Store 后端复用。
func Trim(msgs []*schema.Message, window int) []*schema.Message {
	if window > 0 && len(msgs) > window {
		return msgs[len(msgs)-window:]
	}
	return msgs
}
