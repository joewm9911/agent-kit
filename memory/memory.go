// Package memory 提供长期记忆:以 memory_save / memory_search 能力
// 暴露给模型,由大脑决定何时记、何时查——这是"模型自主"的部分。
// (会话级短期记忆见 session 包,由运行时自动织入,模型无感知。)
//
// KV 后端可替换为 Redis / 向量库,接口不变。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

// KV 是长期记忆的最小契约,Search 可以是关键词匹配,也可以是向量检索。
type KV interface {
	Put(ctx context.Context, key, value string) error
	Search(ctx context.Context, query string, limit int) (map[string]string, error)
}

// Factory 按配置构造长期记忆后端。
type Factory func(conf map[string]any) (KV, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册后端类型(redis/向量库/自定义),实现方空导入即可在
// 配置里以 long_term_store: <type> 引用。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("memory: kv type %q already registered", typ))
	}
	factories[typ] = f
}

func init() {
	Register("inmemory", func(_ map[string]any) (KV, error) {
		return NewInMemoryKV(), nil
	})
}

// New 按类型构造长期记忆后端,空类型默认 inmemory。
func New(typ string, conf map[string]any) (KV, error) {
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
		return nil, fmt.Errorf("memory: unknown kv type %q, registered: %v", typ, names)
	}
	return f(conf)
}

// NewInMemoryKV 返回进程内关键词匹配的长期记忆,适合开发调试。
func NewInMemoryKV() KV {
	return &memKV{data: map[string]string{}}
}

type memKV struct {
	mu   sync.RWMutex
	data map[string]string
}

func (m *memKV) Put(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memKV) Search(_ context.Context, query string, limit int) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	q := strings.ToLower(query)
	for k, v := range m.data {
		if strings.Contains(strings.ToLower(k), q) || strings.Contains(strings.ToLower(v), q) {
			out[k] = v
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// AsCapabilities 把长期记忆包装成 memory_save / memory_search 两个能力。
// save 是改动性操作但风险可控,标记 readonly 以免每次记录都触发审批;
// 接入敏感存储时可自行用 loop.GateApproval 收紧。
func AsCapabilities(kv KV) []capability.Capability {
	save := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "memory", Provider: "builtin", Namespace: "builtin", Name: "memory_save"},
		Description: "保存一条长期记忆。当用户告知偏好、事实或值得跨会话记住的信息时调用。",
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"key":   {Type: schema.String, Desc: "记忆的简短标题", Required: true},
			"value": {Type: schema.String, Desc: "记忆内容", Required: true},
		}),
	}, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct{ Key, Value string }
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", err
		}
		if err := kv.Put(ctx, args.Key, args.Value); err != nil {
			return "", err
		}
		return "saved", nil
	})

	search := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "memory", Provider: "builtin", Namespace: "builtin", Name: "memory_search"},
		Description: "按关键词检索长期记忆。回答依赖用户历史偏好或既往事实时先调用。",
		Params:      capability.SingleParam("query", "检索关键词"),
	}, func(ctx context.Context, argsJSON string) (string, error) {
		query := capability.ParseSingle(argsJSON, "query")
		hits, err := kv.Search(ctx, query, 5)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "no memory found", nil
		}
		var sb strings.Builder
		for k, v := range hits {
			fmt.Fprintf(&sb, "- %s: %s\n", k, v)
		}
		return sb.String(), nil
	})

	return []capability.Capability{save, search}
}
