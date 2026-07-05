// Package redis 提供 redis 后端:store.KV(todo/digest 的原子读改写)与
// session 会话历史。store.KV 是低层原语、随会话后端一并寄居于此(共用连接);
// 长期记忆的 redis 在 impl/memory/redis。空导入即为多副本 serving / 进程
// 重启开启分布式一致性。
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	goredis "github.com/redis/go-redis/v9"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/protocol/session"
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
	session.Register("redis", func(conf map[string]any, window int) (session.Store, error) {
		rdb, prefix, err := redisconn.Dial(conf)
		if err != nil {
			return nil, err
		}
		return &sessStore{rdb: rdb, prefix: prefix + "sess:", window: window}, nil
	})
}

// ---- KV 家族(todo/result）----

type kv struct {
	rdb    *goredis.Client
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

// ---- session 会话历史 ----

type sessStore struct {
	rdb    *goredis.Client
	prefix string
	window int
}

func (s *sessStore) key(sid string) string { return s.prefix + sid }

func (s *sessStore) Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	vals := make([]any, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		vals = append(vals, b)
	}
	return s.rdb.RPush(ctx, s.key(sessionID), vals...).Err()
}

// Load 返回窗口裁剪后的最近消息(window<=0 不裁剪)。
func (s *sessStore) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	start := int64(0)
	if s.window > 0 {
		start = int64(-s.window)
	}
	return s.rangeMsgs(ctx, sessionID, start, -1)
}

// LoadAll 返回全量历史(FullLoader:滚动摘要持久化与会话内召回需要)。
func (s *sessStore) LoadAll(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return s.rangeMsgs(ctx, sessionID, 0, -1)
}

func (s *sessStore) rangeMsgs(ctx context.Context, sessionID string, start, stop int64) ([]*schema.Message, error) {
	raws, err := s.rdb.LRange(ctx, s.key(sessionID), start, stop).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*schema.Message, 0, len(raws))
	for _, r := range raws {
		var m schema.Message
		if err := json.Unmarshal([]byte(r), &m); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, nil
}

func (s *sessStore) Clear(ctx context.Context, sessionID string) error {
	return s.rdb.Del(ctx, s.key(sessionID)).Err()
}
