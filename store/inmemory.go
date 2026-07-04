package store

import (
	"context"
	"strings"
	"sync"
	"time"
)

// InMemory 是进程内 KV 后端:一把互斥锁保证 Update 的原子读改写,
// TTL 惰性过期。是默认后端,也是 redis 等外置后端的语义参照。
type InMemory struct {
	mu   sync.Mutex
	data map[string]entry
}

type entry struct {
	val []byte
	exp time.Time // 零值表示不过期
}

// NewInMemory 构造一个空的进程内 KV。
func NewInMemory() *InMemory {
	return &InMemory{data: map[string]entry{}}
}

// live 在持锁前提下读取未过期的值,顺带惰性清除过期项。
func (m *InMemory) live(key string) ([]byte, bool) {
	e, ok := m.data[key]
	if !ok {
		return nil, false
	}
	if !e.exp.IsZero() && time.Now().After(e.exp) {
		delete(m.data, key)
		return nil, false
	}
	return e.val, true
}

func (m *InMemory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.live(key)
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil // 副本,防外部改动内部状态
}

func (m *InMemory) Update(_ context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error {

	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.live(key)
	var oldCopy []byte
	if ok {
		oldCopy = append([]byte(nil), old...)
	}
	next, err := mutate(oldCopy, ok)
	if err != nil {
		return err
	}
	if next == nil { // 约定:返回 nil 表示删除
		delete(m.data, key)
		return nil
	}
	e := entry{val: append([]byte(nil), next...)}
	if ttl > 0 {
		e.exp = time.Now().Add(ttl)
	}
	m.data[key] = e
	return nil
}

func (m *InMemory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *InMemory) Scan(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if _, ok := m.live(k); !ok { // 跳过并清除过期项
			continue
		}
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
