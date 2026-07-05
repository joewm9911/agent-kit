package loop

import "container/list"

// lru 是进程内会话态的有界容器:容量满时淘汰最久未访问的键。
// 预算账目、审批决策记忆这类"按会话累积"的运行态用它兜住内存上界
// (此前是无限增长的裸 map / 超限全清)。非并发安全,调用方持锁。
//
// 淘汰语义:被淘汰的会话再回来时从零开始(预算重计、审批重问)——
// 只有最久未活跃的会话会被淘汰,失败模式是保守安全的。
type lru[V any] struct {
	max int
	m   map[string]*list.Element
	l   *list.List // front = 最近使用
}

type lruEntry[V any] struct {
	key string
	val V
}

func newLRU[V any](max int) *lru[V] {
	return &lru[V]{max: max, m: map[string]*list.Element{}, l: list.New()}
}

// get 返回键值并将其标记为最近使用。
func (c *lru[V]) get(key string) (V, bool) {
	if e, ok := c.m[key]; ok {
		c.l.MoveToFront(e)
		return e.Value.(*lruEntry[V]).val, true
	}
	var zero V
	return zero, false
}

// put 写入键值(已存在则更新并前移);超容量时淘汰队尾。
func (c *lru[V]) put(key string, val V) {
	if e, ok := c.m[key]; ok {
		e.Value.(*lruEntry[V]).val = val
		c.l.MoveToFront(e)
		return
	}
	c.m[key] = c.l.PushFront(&lruEntry[V]{key: key, val: val})
	if c.l.Len() > c.max {
		tail := c.l.Back()
		c.l.Remove(tail)
		delete(c.m, tail.Value.(*lruEntry[V]).key)
	}
}
