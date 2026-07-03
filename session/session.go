// Package session 提供会话历史的持久化存储。历史织入由 agent 运行时
// 自动完成(模型无感知)——该不该带历史不是模型的决策,是流程保证。
// 后端可替换(进程内/文件/Redis/DB),接口不变。
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Store 是会话历史存储的最小契约。Load 返回窗口裁剪后的最近消息。
type Store interface {
	Load(ctx context.Context, sessionID string) ([]*schema.Message, error)
	Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error
	Clear(ctx context.Context, sessionID string) error
}

// FullLoader 是可选扩展:返回不裁剪的全量历史。实现它的后端可享受
// 滚动摘要持久化与会话内相关性召回;内置 inmemory/file 均已实现。
type FullLoader interface {
	LoadAll(ctx context.Context, sessionID string) ([]*schema.Message, error)
}

// Factory 按配置构造存储。window 为保留的最近消息条数,<=0 不裁剪
// (裁剪只影响 Load 结果,持久化后端应保留全量记录)。
type Factory func(conf map[string]any, window int) (Store, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册存储类型(redis/db/自定义),与 source/prompt/channel
// 的工厂机制同构:实现方空导入即可在配置里以 store: <type> 引用。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("session: store type %q already registered", typ))
	}
	factories[typ] = f
}

func init() {
	Register("inmemory", func(_ map[string]any, window int) (Store, error) {
		return NewInMemory(window), nil
	})
	Register("file", func(conf map[string]any, window int) (Store, error) {
		dir, _ := conf["dir"].(string)
		if dir == "" {
			return nil, fmt.Errorf("session: file store requires dir")
		}
		return NewFileStore(dir, window)
	})
}

// New 按类型构造存储,空类型默认 inmemory。
func New(typ string, conf map[string]any, window int) (Store, error) {
	if typ == "" {
		typ = "inmemory"
	}
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		names := make([]string, 0, len(factories))
		for k := range factories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("session: unknown store type %q, registered: %v", typ, names)
	}
	return f(conf, window)
}

// ---- inmemory ----

// NewInMemory 返回进程内滑动窗口存储,适合开发与无状态短会话。
func NewInMemory(window int) Store {
	return &inMemory{window: window, sessions: map[string][]*schema.Message{}}
}

type inMemory struct {
	mu       sync.RWMutex
	window   int
	sessions map[string][]*schema.Message
}

func (b *inMemory) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	all, err := b.LoadAll(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return trim(all, b.window), nil
}

func (b *inMemory) LoadAll(_ context.Context, sessionID string) ([]*schema.Message, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs := b.sessions[sessionID]
	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (b *inMemory) Append(_ context.Context, sessionID string, msgs ...*schema.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 保留全量,窗口裁剪在 Load 时做(与 file 后端语义一致,
	// 供滚动摘要与相关性召回使用)。
	b.sessions[sessionID] = append(b.sessions[sessionID], msgs...)
	return nil
}

func (b *inMemory) Clear(_ context.Context, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, sessionID)
	return nil
}

// ---- file:每会话一个 JSONL 文件,进程重启后会话可恢复 ----

// NewFileStore 返回文件存储,dir 下每个会话一个 <id>.jsonl。
func NewFileStore(dir string, window int) (Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create dir: %w", err)
	}
	return &fileStore{dir: dir, window: window}, nil
}

type fileStore struct {
	mu     sync.Mutex
	dir    string
	window int
}

func (f *fileStore) path(sessionID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, sessionID)
	return filepath.Join(f.dir, safe+".jsonl")
}

func (f *fileStore) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	all, err := f.LoadAll(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return trim(all, f.window), nil
}

func (f *fileStore) LoadAll(_ context.Context, sessionID string) ([]*schema.Message, error) {
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

func (f *fileStore) Append(_ context.Context, sessionID string, msgs ...*schema.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.path(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := file.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func (f *fileStore) Clear(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func trim(msgs []*schema.Message, window int) []*schema.Message {
	if window > 0 && len(msgs) > window {
		return msgs[len(msgs)-window:]
	}
	return msgs
}
