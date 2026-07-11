package memory

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// testStore 是测试用的进程内关键词后端(具体 inmemory 后端在
// impl/memory/inmemory,内部测试不便引入以免成环,这里放一份等价 fixture)。
func newTestStore() Store { return &testStore{buckets: map[string]map[string]string{}} }

type testStore struct {
	mu      sync.RWMutex
	buckets map[string]map[string]string
}

func (m *testStore) Put(_ context.Context, scope, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[scope] == nil {
		m.buckets[scope] = map[string]string{}
	}
	m.buckets[scope][key] = value
	return nil
}

func (m *testStore) Search(_ context.Context, scopes []string, query string, limit int) ([]Hit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Hit
	for _, scope := range scopes {
		hits := ScanBucket(scope, m.buckets[scope], query)
		SortHits(hits)
		out = append(out, hits...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

func saveSearch(t *testing.T, kv Store, scope ScopeConfig) (save, search capability.Capability) {
	t.Helper()
	caps := AsCapabilities(kv, scope)
	return caps[0], caps[1]
}

func userCtx(id string) context.Context {
	return runctx.WithUser(runctx.With(context.Background(), "a", "s"), id)
}

// TestUserScopeIsolation 验证用户级隔离:A 写入的记忆 B 检索不到。
func TestUserScopeIsolation(t *testing.T) {
	kv := newTestStore()
	save, search := saveSearch(t, kv, ScopeConfig{}) // 缺省:写 user、读 user+shared

	// 用户 A 记下偏好
	if out, err := capability.Invoke(userCtx("A"), save,
		`{"key":"汇报偏好","value":"喜欢简短汇报"}`); err != nil || out != "saved" {
		t.Fatalf("A save: %q %v", out, err)
	}
	// A 检索得到
	if out, _ := capability.Invoke(userCtx("A"), search, `{"query":"偏好"}`); !strings.Contains(out, "简短") {
		t.Fatalf("A should see own memory: %q", out)
	}
	// B 检索不到 A 的
	if out, _ := capability.Invoke(userCtx("B"), search, `{"query":"偏好"}`); strings.Contains(out, "简短") {
		t.Fatalf("B must not see A's memory: %q", out)
	}
}

// TestSharedReadableNotWritable 验证读放开写收窄:用户面 agent 读得到
// 共享池,但对话写入落用户桶、进不了共享池。
func TestSharedReadableNotWritable(t *testing.T) {
	kv := newTestStore()
	// 运维侧(非对话)灌入共享知识
	_ = kv.Put(context.Background(), SharedScope, "发布流程", "灰度→观察→全量")

	save, search := saveSearch(t, kv, ScopeConfig{}) // 默认 write=user

	// 用户读得到共享池
	if out, _ := capability.Invoke(userCtx("A"), search, `{"query":"发布"}`); !strings.Contains(out, "灰度") {
		t.Fatalf("shared should be readable: %q", out)
	}
	// 用户对话写入,即便内容像"通用规范",也落用户桶而非共享池
	if _, err := capability.Invoke(userCtx("A"), save,
		`{"key":"公司通用规范","value":"这是给所有人的"}`); err != nil {
		t.Fatal(err)
	}
	// 共享池未被污染:另一个用户 B 检索不到 A 那条
	if out, _ := capability.Invoke(userCtx("B"), search, `{"query":"通用规范"}`); strings.Contains(out, "所有人") {
		t.Fatalf("conversational write must not reach shared pool: %q", out)
	}
}

// TestWriteScopeSharedForPrivilegedAgent 验证特权 agent:显式配
// write_scope: shared 时,对话写入落共享池,对所有用户可见。
func TestWriteScopeSharedForPrivilegedAgent(t *testing.T) {
	kv := newTestStore()
	save, _ := saveSearch(t, kv, ScopeConfig{Write: SharedScope})
	if _, err := capability.Invoke(userCtx("admin"), save,
		`{"key":"新流程","value":"已更新"}`); err != nil {
		t.Fatal(err)
	}
	// 另一个用户(缺省策略)读得到
	_, search2 := saveSearch(t, kv, ScopeConfig{})
	if out, _ := capability.Invoke(userCtx("someone"), search2, `{"query":"新流程"}`); !strings.Contains(out, "已更新") {
		t.Fatalf("privileged shared write should be visible to all: %q", out)
	}
}

// TestNoUserIdentityFailsFast 验证无用户身份时用户记忆写入 fail fast,
// 以工具结果告知而非静默落进共享池。
func TestNoUserIdentityFailsFast(t *testing.T) {
	kv := newTestStore()
	save, _ := saveSearch(t, kv, ScopeConfig{}) // write=user

	noUser := runctx.With(context.Background(), "a", "s") // 无 WithUser
	out, err := capability.Invoke(noUser, save, `{"key":"x","value":"y"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "end-user identity") {
		t.Fatalf("expect fail-fast message, got %q", out)
	}
	// 确认没有落进任何桶(共享池也没有)
	hits, _ := kv.Search(context.Background(), []string{SharedScope}, "x", 5)
	if len(hits) != 0 {
		t.Fatal("failed write must not leak into shared pool")
	}
}

// TestReadScopesConfigured 验证 read_scopes 可裁剪(如只读共享、不读用户桶)。
func TestReadScopesConfigured(t *testing.T) {
	kv := newTestStore()
	_ = kv.Put(context.Background(), UserScope("A"), "私密", "用户私有")
	_ = kv.Put(context.Background(), SharedScope, "公开", "共享内容")

	_, searchSharedOnly := saveSearch(t, kv, ScopeConfig{Read: []string{SharedScope}})
	out, _ := capability.Invoke(userCtx("A"), searchSharedOnly, `{"query":"私密"}`)
	if strings.Contains(out, "用户私有") {
		t.Fatalf("read_scopes=[shared] must not reach user bucket: %q", out)
	}
	out, _ = capability.Invoke(userCtx("A"), searchSharedOnly, `{"query":"公开"}`)
	if !strings.Contains(out, "共享内容") {
		t.Fatalf("shared still readable: %q", out)
	}
}
