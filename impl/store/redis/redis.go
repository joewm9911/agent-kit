// Package redis 提供 store.KV 的 redis 后端:WATCH/MULTI 乐观锁保证
// Update 的原子读改写。todo/digest/suspend/budget/approval 等一切落在
// store.KV 上的状态,空导入本包即获得多副本分布式一致性;会话历史的
// redis 在 impl/session/redis,长期记忆的在 impl/memory/redis(三者共用
// impl/utils/redisconn 的连接构造;公司自有客户端封装经
// redisconn.RegisterClient 注册后,配置以 client: <name> 引用)。
package redis

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

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

// ---- KV 家族(todo/result）----

type kv struct {
	rdb    goredis.UniversalClient
	prefix string
}

func (k *kv) key(s string) string { return k.prefix + s }

func (k *kv) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := k.rdb.Get(ctx, k.key(key)).Bytes()
	if err == goredis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// updateRetries 是乐观锁冲突的重试上限;冲突时带抖动退避,避免多写者
// 锁步碰撞而饿死。真实用例(todo/result 按会话分键)几乎无同键并发,
// 上限主要为对抗性场景兜底。
const updateRetries = 200

func (k *kv) Update(ctx context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error {

	rk := k.key(key)
	txf := func(tx *goredis.Tx) error {
		old, err := tx.Get(ctx, rk).Bytes()
		ok := true
		if err == goredis.Nil {
			ok, old = false, nil
		} else if err != nil {
			return err
		}
		next, err := mutate(old, ok)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p goredis.Pipeliner) error {
			if next == nil {
				p.Del(ctx, rk)
			} else {
				p.Set(ctx, rk, next, ttl) // ttl<=0 → 0 → 无过期
			}
			return nil
		})
		return err
	}
	for i := 0; i < updateRetries; i++ {
		err := k.rdb.Watch(ctx, txf, rk)
		if err == goredis.TxFailedErr { // EXEC 冲突,抖动退避后重试
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
			continue
		}
		return err
	}
	return fmt.Errorf("redis: update %q: too much contention", key)
}

// backoff 返回第 i 次冲突的退避时长:线性增长上限 2ms + 满抖动,
// 打散锁步的写者。
func backoff(i int) time.Duration {
	base := time.Duration(i+1) * 100 * time.Microsecond
	if base > 2*time.Millisecond {
		base = 2 * time.Millisecond
	}
	return base/2 + time.Duration(rand.Int63n(int64(base/2)+1))
}

func (k *kv) Delete(ctx context.Context, key string) error {
	return k.rdb.Del(ctx, k.key(key)).Err()
}

func (k *kv) Scan(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	iter := k.rdb.Scan(ctx, 0, k.key(prefix)+"*", 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, strings.TrimPrefix(iter.Val(), k.prefix))
	}
	return keys, iter.Err()
}
