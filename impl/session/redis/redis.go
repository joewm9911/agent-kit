// Package redis 提供 session 会话历史的 redis 后端(RPUSH/LRANGE 追加日志,
// 实现 FullLoader 支持滚动摘要与窗外召回)。store.KV 的 redis 在
// impl/store/redis,长期记忆的在 impl/memory/redis(共用 redisconn)。
package redis

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/protocol/session"
)

func init() {
	session.Register("redis", func(conf map[string]any, window int) (session.Store, error) {
		rdb, prefix, err := redisconn.Dial(conf)
		if err != nil {
			return nil, err
		}
		return &sessStore{rdb: rdb, prefix: prefix + "sess:", window: window}, nil
	})
}

// ---- session 会话历史 ----

type sessStore struct {
	rdb    redisconn.Client
	prefix string
	window int
}

func (s *sessStore) key(sid string) string { return s.prefix + sid }

func (s *sessStore) Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	vals := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		vals = append(vals, b)
	}
	return s.rdb.RPush(ctx, s.key(sessionID), vals...)
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
	raws, err := s.rdb.LRange(ctx, s.key(sessionID), start, stop)
	if err != nil {
		return nil, err
	}
	out := make([]*schema.Message, 0, len(raws))
	for _, r := range raws {
		var m schema.Message
		if err := json.Unmarshal(r, &m); err != nil {
			// 一条损坏(redis OOM 截断/外部误写)不砖死整个会话:跳过留痕,
			// 与 file 后端的坏行容忍一致。否则该会话此后每轮 load 必败。
			slog.Warn("session/redis: skip corrupt entry", "session", sessionID, "err", err)
			continue
		}
		out = append(out, &m)
	}
	return out, nil
}

func (s *sessStore) Clear(ctx context.Context, sessionID string) error {
	return s.rdb.Delete(ctx, s.key(sessionID))
}
