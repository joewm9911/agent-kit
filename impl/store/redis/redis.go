// Package redis 提供 store.KV 的 redis 后端。todo/digest/suspend/budget/
// approval 等一切落在 store.KV 上的状态,空导入本包即获得多副本分布式
// 一致性;会话历史的 redis 在 impl/session/redis,长期记忆的在
// impl/memory/redis。三者只消费 redisconn.Client 能力面接口——原子读改写
// 等语义契约由接口实现承担(官方 go-redis 实现用 WATCH/MULTI 乐观锁,
// 公司自有封装实现同一接口即可整体替换,配置 client: <name> 引用)。
package redis

import (
	"context"
	"strings"
	"time"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/protocol/store"
)

func init() {
	store.RegisterBackend("redis", func(conf map[string]any) (store.KV, error) {
		rdb, prefix, err := redisconn.Dial(conf)
		if err != nil {
			return nil, err
		}
		return &kv{rdb: rdb, prefix: prefix}, nil
	})
}

// ---- KV 家族(todo/result)----

type kv struct {
	rdb    redisconn.Client
	prefix string
}

func (k *kv) key(s string) string { return k.prefix + s }

func (k *kv) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return k.rdb.Get(ctx, k.key(key))
}

func (k *kv) Update(ctx context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error {
	return k.rdb.Update(ctx, k.key(key), mutate, ttl)
}

func (k *kv) Delete(ctx context.Context, key string) error {
	return k.rdb.Delete(ctx, k.key(key))
}

func (k *kv) Scan(ctx context.Context, prefix string) ([]string, error) {
	keys, err := k.rdb.Scan(ctx, k.key(prefix))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, strings.TrimPrefix(key, k.prefix))
	}
	return out, nil
}
