// Package redis 提供长期记忆的 redis 后端:每个 scope 落一个 redis hash
// (<prefix>mem:<scope>,field=key、value=value),跨副本共享。检索沿用
// inmemory 的关键词匹配语义;向量检索是另一家族(vectorstore),由外部
// 向量库提供。空导入即启用。
package redis

import (
	"context"
	"strings"

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

// Search 在给定 scopes 内做关键词匹配,命中满 limit 即返回。多个 scope 出现
// 同名 key 时,后遍历的 scope 覆盖(对齐 inmemory 的 out[k]=v 语义)。
func (m *memStore) Search(ctx context.Context, scopes []string, query string, limit int) (map[string]string, error) {
	out := map[string]string{}
	q := strings.ToLower(query)
	for _, scope := range scopes {
		all, err := m.rdb.HGetAll(ctx, m.key(scope))
		if err != nil {
			return nil, err
		}
		for k, v := range all {
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
