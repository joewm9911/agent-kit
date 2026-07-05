// Package inmemory 是 memory 的进程内关键词匹配后端(memory type: inmemory),
// 按 scope 分桶。开发/测试/无外部向量库时可用。空导入(或经 agent-kit/std)
// 即注册。
package inmemory

import (
	"context"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/memory"
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

func (m *store) Search(_ context.Context, scopes []string, query string, limit int) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	q := strings.ToLower(query)
	for _, scope := range scopes {
		for k, v := range m.buckets[scope] {
			if strings.Contains(strings.ToLower(k), q) || strings.Contains(strings.ToLower(v), q) {
				out[k] = v
				if limit > 0 && len(out) >= limit {
					return out, nil
				}
			}
		}
	}
	return out, nil
}
