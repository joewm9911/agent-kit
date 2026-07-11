// Package redis 提供长期记忆的 redis 后端:每个 scope 落一个 redis hash
// (<prefix>mem:<scope>,field=key、value=value),跨副本共享。检索沿用
// inmemory 的关键词匹配语义;向量检索是另一家族(vectorstore),由外部
// 向量库提供。空导入即启用。
package redis

import (
	"context"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/protocol/memory"
)

func init() {
	memory.Register("redis", func(conf map[string]any) (memory.Store, error) {
		rdb, prefix, err := redisconn.Dial(conf)
		if err != nil {
			return nil, err
		}
		return &memStore{rdb: rdb, prefix: prefix + "mem:"}, nil
	})
}

type memStore struct {
	rdb    redisconn.Client
	prefix string
}

func (m *memStore) key(scope string) string { return m.prefix + scope }

func (m *memStore) Put(ctx context.Context, scope, key, value string) error {
	return m.rdb.HSet(ctx, m.key(scope), key, value)
}

// Search 按 scopes 顺序检索(先 user 后 shared 即优先级),scope 内按
// 相关度降序;满 limit 截断。整 hash HGetAll 后本地打分——规模到需要
// 服务端检索时应换向量后端(vectorstore 家族),不在这层做 SCAN 优化。
func (m *memStore) Search(ctx context.Context, scopes []string, query string, limit int) ([]memory.Hit, error) {
	var out []memory.Hit
	for _, scope := range scopes {
		all, err := m.rdb.HGetAll(ctx, m.key(scope))
		if err != nil {
			return nil, err
		}
		hits := memory.ScanBucket(scope, all, query)
		memory.SortHits(hits)
		out = append(out, hits...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}
