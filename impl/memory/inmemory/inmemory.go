// Package inmemory 是 memory 的进程内关键词匹配后端(memory type: inmemory),
// 按 scope 分桶。开发/测试/无外部向量库时可用。空导入(或经 agent-kit/std)
// 即注册。
package inmemory

import (
	"context"
	"sync"

	"github.com/joewm9911/agent-kit/protocol/memory"
)

func init() {
	memory.Register("inmemory", func(_ map[string]any) (memory.Store, error) {
		return New(), nil
	})
}

// New 返回进程内关键词匹配的长期记忆,按 scope 分桶。
func New() memory.Store {
	return &store{buckets: map[string]map[string]string{}}
}

type store struct {
	mu      sync.RWMutex
	buckets map[string]map[string]string // scope → (key → value)
}

func (m *store) Put(_ context.Context, scope, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[scope]
	if b == nil {
		b = map[string]string{}
		m.buckets[scope] = b
	}
	b[key] = value
	return nil
}

// Search 按 scopes 顺序检索(先 user 后 shared 即优先级),scope 内按
// 相关度降序;满 limit 截断——先给的 scope 挤掉后给的,不做跨 scope 覆盖。
func (m *store) Search(_ context.Context, scopes []string, query string, limit int) ([]memory.Hit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []memory.Hit
	for _, scope := range scopes {
		hits := memory.ScanBucket(scope, m.buckets[scope], query)
		memory.SortHits(hits)
		out = append(out, hits...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}
