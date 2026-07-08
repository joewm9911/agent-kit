// Package redisconntest 提供 redisconn.Client 的内存实现:测试里替代
// 真 redis,同时是第三方实现 Client 接口的最小参考——每个方法的语义
// 契约(Update 的原子性、Get 的 ok 语义、LRange 的负下标)在此逐一体现。
package redisconntest

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
)

// New 返回内存版 Client。ttl 被记录但不主动过期(测试不依赖时钟)。
func New() redisconn.Client {
	return &memClient{
		kv:     map[string][]byte{},
		lists:  map[string][][]byte{},
		hashes: map[string]map[string]string{},
	}
}

type memClient struct {
	mu     sync.Mutex
	kv     map[string][]byte
	lists  map[string][][]byte
	hashes map[string]map[string]string
}

func (m *memClient) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// Update 的原子性靠单把锁(第三方生产实现换成 Lua/CAS/事务,契约相同:
// mutate 期间无并发写者插入,nil 返回值删键)。
func (m *memClient) Update(_ context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.kv[key]
	next, err := mutate(old, ok)
	if err != nil {
		return err
	}
	if next == nil {
		delete(m.kv, key)
		return nil
	}
	m.kv[key] = append([]byte(nil), next...)
	return nil
}

func (m *memClient) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.kv, key)
	delete(m.lists, key)
	delete(m.hashes, key)
	return nil
}

func (m *memClient) Scan(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.kv {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (m *memClient) RPush(_ context.Context, key string, vals ...[]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range vals {
		m.lists[key] = append(m.lists[key], append([]byte(nil), v...))
	}
	return nil
}

func (m *memClient) LRange(_ context.Context, key string, start, stop int64) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lists[key]
	n := int64(len(l))
	if start < 0 {
		start += n
	}
	if stop < 0 {
		stop += n
	}
	if start < 0 {
		start = 0
	}
	if stop >= n {
		stop = n - 1
	}
	if start > stop || n == 0 {
		return nil, nil
	}
	out := make([][]byte, 0, stop-start+1)
	for _, v := range l[start : stop+1] {
		out = append(out, append([]byte(nil), v...))
	}
	return out, nil
}

func (m *memClient) HSet(_ context.Context, key, field, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	m.hashes[key][field] = value
	return nil
}

func (m *memClient) HGetAll(_ context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.hashes[key]))
	for k, v := range m.hashes[key] {
		out[k] = v
	}
	return out, nil
}
