// Package file 是 session 的文件后端(session type: file):dir 下每个会话
// 一个 <id>.jsonl,进程重启后会话可恢复。空导入(或经 agent-kit/std)即注册。
package file

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/protocol/session"
)

func init() {
	session.Register("file", func(conf map[string]any, window int) (session.Store, error) {
		dir, ok := conf["dir"].(string)
		if conf["dir"] != nil && !ok {
			return nil, fmt.Errorf("session: file store dir must be a string, got %T", conf["dir"])
		}
		if dir == "" {
			return nil, fmt.Errorf("session: file store requires dir")
		}
		return New(dir, window)
	})
}

// New 返回文件存储,dir 下每个会话一个 <id>.jsonl。
func New(dir string, window int) (session.Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create dir: %w", err)
	}
	return &store{dir: dir, window: window}, nil
}

type store struct {
	mu     sync.Mutex
	dir    string
	window int
}

// path 生成会话文件路径。sanitize 把非法字符映射为 '_' 会产生碰撞
// ("a/b" 与 "a_b" 同名 → 会话串线),因此凡是被改写过的 ID 追加原始
// ID 的哈希后缀;本来就安全的 ID 保持原名(兼容既有文件)。
func (f *store) path(sessionID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, sessionID)
	if safe == sessionID {
		return filepath.Join(f.dir, safe+".jsonl")
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(f.dir, fmt.Sprintf("%s-%x.jsonl", safe, sum[:4]))
}

func (f *store) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	all, err := f.LoadAll(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return session.Trim(all, f.window), nil
}

func (f *store) LoadAll(_ context.Context, sessionID string) ([]*schema.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.Open(f.path(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var msgs []*schema.Message
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var m schema.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue // 容忍坏行,不让单条脏数据毁掉整个会话
		}
		msgs = append(msgs, &m)
	}
	return msgs, sc.Err()
}

func (f *store) Append(_ context.Context, sessionID string, msgs ...*schema.Message) error {
	// 先整体序列化再单次写入:一轮的多条消息尽量原子落盘。
	var buf []byte
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.path(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(buf)
	return err
}

func (f *store) Clear(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
