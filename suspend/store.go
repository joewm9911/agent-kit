package suspend

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// NewFileStore 创建文件后端:<dir>/<kind>/<key> 一条记录一个文件,
// 进程重启后可读回。key 做十六进制编码,避免路径注入与非法字符。
func NewFileStore(dir string) (Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("suspend: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileStore{dir: dir}, nil
}

type fileStore struct {
	mu  sync.Mutex
	dir string
}

func (f *fileStore) path(kind, key string) string {
	return filepath.Join(f.dir, kind, hex.EncodeToString([]byte(key)))
}

func (f *fileStore) Put(kind, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(f.dir, kind), 0o755); err != nil {
		return err
	}
	tmp := f.path(kind, key) + ".tmp"
	if err := os.WriteFile(tmp, value, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.path(kind, key))
}

func (f *fileStore) Get(kind, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(f.path(kind, key))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (f *fileStore) Delete(kind, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(kind, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *fileStore) List(kind string) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(f.dir, kind))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		keyBytes, err := hex.DecodeString(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, kind, e.Name()))
		if err != nil {
			continue
		}
		out[string(keyBytes)] = raw
	}
	return out, nil
}

// NewInMemory 创建内存后端(测试用,不跨进程)。
func NewInMemory() Store {
	return &memStore{data: map[string]map[string][]byte{}}
}

type memStore struct {
	mu   sync.Mutex
	data map[string]map[string][]byte
}

func (m *memStore) Put(kind, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[kind] == nil {
		m.data[kind] = map[string][]byte{}
	}
	m.data[kind][key] = append([]byte(nil), value...)
	return nil
}

func (m *memStore) Get(kind, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[kind][key]
	return v, ok, nil
}

func (m *memStore) Delete(kind, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data[kind], key)
	return nil
}

func (m *memStore) List(kind string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range m.data[kind] {
		out[k] = v
	}
	return out, nil
}
