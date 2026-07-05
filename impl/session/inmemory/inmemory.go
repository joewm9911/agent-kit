// Package inmemory 是 session 的进程内滑动窗口后端(session type: inmemory),
// 适合开发与无状态短会话。空导入(或经 agent-kit/std)即注册。
package inmemory

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/session"
)

func init() {
	session.Register("inmemory", func(_ map[string]any, window int) (session.Store, error) {
		return New(window), nil
	})
}

// maxInMemorySessions 是会话数上限:开发后端不该无界增长,超限时淘汰最久
// 未活跃的会话(生产请用 file/redis/自定义后端)。
const maxInMemorySessions = 1024

// New 返回进程内滑动窗口存储。
func New(window int) session.Store {
	return &store{window: window, sessions: map[string][]*schema.Message{}, touch: map[string]int64{}}
}

type store struct {
	mu       sync.RWMutex
	window   int
	sessions map[string][]*schema.Message
	touch    map[string]int64 // 会话 → 活跃序号(LRU 淘汰用)
	seq      int64
}

func (b *store) evictLocked() {
	for len(b.sessions) > maxInMemorySessions {
		oldest, min := "", int64(1<<62)
		for id, at := range b.touch {
			if at < min {
				oldest, min = id, at
			}
		}
		delete(b.sessions, oldest)
		delete(b.touch, oldest)
	}
}

func (b *store) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	all, err := b.LoadAll(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return session.Trim(all, b.window), nil
}

func (b *store) LoadAll(_ context.Context, sessionID string) ([]*schema.Message, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs := b.sessions[sessionID]
	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (b *store) Append(_ context.Context, sessionID string, msgs ...*schema.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 保留全量,窗口裁剪在 Load 时做(与 file 后端语义一致)。
	b.sessions[sessionID] = append(b.sessions[sessionID], msgs...)
	b.seq++
	b.touch[sessionID] = b.seq
	b.evictLocked()
	return nil
}

func (b *store) Clear(_ context.Context, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, sessionID)
	delete(b.touch, sessionID)
	return nil
}
