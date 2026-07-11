// Package file 提供 store.KV 的文件后端:一键一文件,写入 tmp+rename
// 原子落盘,进程重启后可读回。单进程内一把互斥锁保证 Update 的原子读改写
// (文件后端定位是单节点持久化,不提供跨进程并发写保证——那是 redis 的场景)。
//
// 配置:{dir: <目录>}。文件名是键的十六进制编码(键可含任意字符),
// 值带 JSON 信封记录过期时间,TTL 惰性过期(读到过期项即删)。
//
// 注册 type = "file":
//
//	stores:
//	  - {name: plans, kind: todo, type: file, config: {dir: ./data/todo}}
package file

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/protocol/store"
)

func init() {
	store.RegisterBackend("file", func(conf map[string]any) (store.KV, error) {
		dir, _ := conf["dir"].(string)
		return New(dir)
	})
}

// New 创建以 dir 为根的文件 KV。
func New(dir string) (store.KV, error) {
	if dir == "" {
		return nil, fmt.Errorf("file store: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileKV{dir: dir}, nil
}

type fileKV struct {
	mu  sync.Mutex
	dir string
}

// envelope 是落盘格式:值 + 可选过期时间(UnixNano,0=不过期)。
type envelope struct {
	Exp int64  `json:"exp,omitempty"`
	Val []byte `json:"val"`
}

func (f *fileKV) path(key string) string {
	return filepath.Join(f.dir, hex.EncodeToString([]byte(key)))
}

// live 在持锁前提下读取未过期的值;过期项顺带删除。
func (f *fileKV) live(key string) ([]byte, bool, error) {
	raw, err := os.ReadFile(f.path(key))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		// 损坏条目隔离(断电撕裂写等):改名保留取证、按不存在返回——
		// 否则该键此后所有 Get/Update 永久失败(如坏掉的 budget 键会把
		// 该会话的模型调用一直 fail-closed),只能人工删文件。
		_ = os.Rename(f.path(key), f.path(key)+".corrupt")
		slog.Warn("file store: quarantined corrupt entry", "key", key, "err", err)
		return nil, false, nil
	}
	if e.Exp > 0 && time.Now().UnixNano() > e.Exp {
		_ = os.Remove(f.path(key))
		return nil, false, nil
	}
	return e.Val, true, nil
}

func (f *fileKV) write(key string, val []byte, ttl time.Duration) error {
	e := envelope{Val: val}
	if ttl > 0 {
		e.Exp = time.Now().Add(ttl).UnixNano()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := f.path(key) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.path(key))
}

func (f *fileKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live(key)
}

func (f *fileKV) Update(_ context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok, err := f.live(key)
	if err != nil {
		return err
	}
	next, err := mutate(old, ok)
	if err != nil {
		return err
	}
	if next == nil { // 约定:返回 nil 表示删除
		err := os.Remove(f.path(key))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return f.write(key, next, ttl)
}

func (f *fileKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *fileKV) Scan(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		kb, err := hex.DecodeString(e.Name())
		if err != nil {
			continue
		}
		key := string(kb)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, ok, _ := f.live(key); !ok { // 跳过并清除过期项
			continue
		}
		keys = append(keys, key)
	}
	return keys, nil
}
