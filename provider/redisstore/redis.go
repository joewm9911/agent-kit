// Package redisstore 提供 redis 后端:一次空导入即为四类存储开启分布式
// 存储——KV 家族(todo/result 计划与暂存)、session 会话历史、memory 长期
// 记忆——多副本 serving / 进程重启下的一致性靠它兜底。
//
// KV.Update 用 WATCH/MULTI 乐观锁做原子读改写(冲突重试),对齐 inmemory
// 的 mutex 语义;session 历史用 redis list(RPUSH/LRANGE);memory 按 scope
// 分桶存 redis hash(HSET/HGETALL),检索沿用关键词匹配(对齐 inmemory 语义,
// 向量检索是另一家族,由 qdrant 等后端提供)。
//
//	stores:
//	  - {name: plans, kind: todo,    type: redis, config: {addr: 127.0.0.1:6379}}
//	  - {name: cache, kind: result,  type: redis, config: {addr: 127.0.0.1:6379}}
//	  - {name: sess,  kind: session, type: redis, config: {addr: 127.0.0.1:6379}}
//	  - {name: ltm,   kind: memory,  type: redis, config: {addr: 127.0.0.1:6379}}
package redisstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"

	"github.com/joewm9911/agent-kit/memory"
	"github.com/joewm9911/agent-kit/session"
	"github.com/joewm9911/agent-kit/store"
)

func init() {
	store.RegisterBackend("redis", func(conf map[string]any) (store.KV, error) {
		rdb, prefix, err := dial(conf)
		if err != nil {
			return nil, err
		}
		return &kv{rdb: rdb, prefix: prefix}, nil
	})
	session.Register("redis", func(conf map[string]any, window int) (session.Store, error) {
		rdb, prefix, err := dial(conf)
		if err != nil {
			return nil, err
		}
		return &sessStore{rdb: rdb, prefix: prefix + "sess:", window: window}, nil
	})
	memory.Register("redis", func(conf map[string]any) (memory.Store, error) {
		rdb, prefix, err := dial(conf)
		if err != nil {
			return nil, err
		}
		return &memStore{rdb: rdb, prefix: prefix + "mem:"}, nil
	})
}

// dial 从配置构造 redis 客户端并连通性自检。config:
// addr(默认 127.0.0.1:6379)· password · db · prefix(键前缀,多租隔离)。
func dial(conf map[string]any) (*redis.Client, string, error) {
	addr, _ := conf["addr"].(string)
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password, _ := conf["password"].(string)
	prefix, _ := conf["prefix"].(string)
	db := 0
	switch v := conf["db"].(type) {
	case int:
		db = v
	case float64:
		db = int(v)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, "", fmt.Errorf("redis: ping %s: %w", addr, err)
	}
	return rdb, prefix, nil
}

// ---- KV 家族(todo/result)----

type kv struct {
	rdb    *redis.Client
	prefix string
}

func (k *kv) key(s string) string { return k.prefix + s }

func (k *kv) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := k.rdb.Get(ctx, k.key(key)).Bytes()
	if err == redis.Nil {
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
	txf := func(tx *redis.Tx) error {
		old, err := tx.Get(ctx, rk).Bytes()
		ok := true
		if err == redis.Nil {
			ok, old = false, nil
		} else if err != nil {
			return err
		}
		next, err := mutate(old, ok)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
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
		if err == redis.TxFailedErr { // EXEC 冲突,抖动退避后重试
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
	rdb    *redis.Client
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

// ---- memory 长期记忆(按 scope 分桶,关键词匹配)----

// memStore 是分布式长期记忆:每个 scope 落一个 redis hash
// (<prefix>mem:<scope>,field=key、value=value),跨副本共享。检索沿用
// inmemory 的关键词匹配语义(key 或 value 含 query 子串,大小写不敏感);
// 向量检索是另一家族(scope→metadata filter),由 qdrant 等后端提供。
type memStore struct {
	rdb    *redis.Client
	prefix string
}

func (m *memStore) key(scope string) string { return m.prefix + scope }

func (m *memStore) Put(ctx context.Context, scope, key, value string) error {
	return m.rdb.HSet(ctx, m.key(scope), key, value).Err()
}

// Search 在给定 scopes 内做关键词匹配,命中满 limit 即返回。多个 scope 出现
// 同名 key 时,后遍历的 scope 覆盖(对齐 memStore 的 out[k]=v 语义)。
func (m *memStore) Search(ctx context.Context, scopes []string, query string, limit int) (map[string]string, error) {
	out := map[string]string{}
	q := strings.ToLower(query)
	for _, scope := range scopes {
		all, err := m.rdb.HGetAll(ctx, m.key(scope)).Result()
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
