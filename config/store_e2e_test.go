package config

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/builtin"
	"github.com/joewm9911/agent-kit/capability"
	_ "github.com/joewm9911/agent-kit/provider/redisstore" // 注册 redis 后端
	"github.com/joewm9911/agent-kit/runctx"
	"github.com/joewm9911/agent-kit/store"
)

func redisConf(t *testing.T) map[string]any {
	t.Helper()
	conf := map[string]any{"addr": "127.0.0.1:6379", "db": 14, "prefix": "akcfg:"}
	kv, err := store.NewBackend("redis", conf)
	if err != nil {
		t.Skipf("redis 不可达,跳过: %v", err)
	}
	// 清场
	keys, _ := kv.Scan(context.Background(), "")
	for _, k := range keys {
		_ = kv.Delete(context.Background(), k)
	}
	return conf
}

// TestTodoStoreCrossReplica 验证 #7 的分布式核心:todo 计划经外置 redis
// 后端,在「副本 A」写入、在「副本 B」(另一个后端客户端)读取仍可见——
// 这正是包级 map 时代跨副本丢计划的场景。
func TestTodoStoreCrossReplica(t *testing.T) {
	conf := redisConf(t)
	t.Cleanup(func() { builtin.SetStore(store.NewInMemory(), 0) }) // 复位全局,不污染其它测试

	caps := builtin.TodoCapabilities()
	write, read := caps[0], caps[1]
	ctx := runctx.With(context.Background(), "ops", "s-42")

	// 副本 A:接入 redis 后端,写计划
	kvA, err := store.NewBackend("redis", conf)
	if err != nil {
		t.Fatal(err)
	}
	builtin.SetStore(kvA, 0)
	if _, err := capability.Invoke(ctx, write, `{"todos":[{"content":"查支付超时","status":"in_progress"},{"content":"出报告","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}

	// 副本 B:另起一个 redis 后端客户端(模拟另一进程/副本),读同一计划
	kvB, err := store.NewBackend("redis", conf)
	if err != nil {
		t.Fatal(err)
	}
	builtin.SetStore(kvB, 0)
	out, err := capability.Invoke(ctx, read, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "查支付超时") || !strings.Contains(out, "出报告") {
		t.Fatalf("跨副本读不到计划(分布式 todo 失效): %q", out)
	}
}

func TestResolveStoreRef(t *testing.T) {
	stores := []StoreInstance{
		{Name: "plans", Kind: "todo", Type: "redis", Config: map[string]any{"addr": "x"}},
		{Name: "sess", Kind: "session", Type: "file"},
	}
	// cap 引用 → 实例
	typ, conf, _, err := resolveStoreRef("cap://store/todo/plans", stores, "todo")
	if err != nil || typ != "redis" || conf["addr"] != "x" {
		t.Fatalf("resolve failed: %s %v %v", typ, conf, err)
	}
	// 裸 type = 缺省简写
	typ, _, _, err = resolveStoreRef("file", stores, "session")
	if err != nil || typ != "file" {
		t.Fatalf("bare type: %s %v", typ, err)
	}
	// kind 槽不符
	if _, _, _, err := resolveStoreRef("cap://store/todo/plans", stores, "session"); err == nil {
		t.Fatal("expected kind-slot mismatch error")
	}
	// 未声明实例
	if _, _, _, err := resolveStoreRef("cap://store/todo/ghost", stores, "todo"); err == nil {
		t.Fatal("expected missing-instance error")
	}
	// 非 store 引用
	if _, _, _, err := resolveStoreRef("cap://retriever/session/x", stores, "session"); err == nil {
		t.Fatal("expected non-store-ref error")
	}
}
