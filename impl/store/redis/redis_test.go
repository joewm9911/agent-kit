package redis

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/impl/utils/redisconn/redisconntest"
	"github.com/joewm9911/agent-kit/protocol/store"
)

// testConf 用独立 db(12,与 session=15/memory=14 分开,防并行 FlushDB 互清)
// 与固定前缀;redis 不可达则跳过。清理直接用 goredis(FlushDB 是测试
// 专属操作,不在 redisconn.Client 能力面里)。
func testConf(t *testing.T) map[string]any {
	t.Helper()
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379", DB: 12})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis 不可达,跳过: %v", err)
	}
	rdb.FlushDB(context.Background())
	t.Cleanup(func() { rdb.FlushDB(context.Background()); rdb.Close() })
	return map[string]any{"addr": "127.0.0.1:6379", "db": 12, "prefix": "akkv:"}
}

func TestRedisKVRoundtrip(t *testing.T) {
	kv, err := store.NewBackend("redis", testConf(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, _ := kv.Get(ctx, "a"); ok {
		t.Fatal("empty miss expected")
	}
	must(t, kv.Update(ctx, "a", func(_ []byte, _ bool) ([]byte, error) { return []byte("hi"), nil }, 0))
	if v, ok, _ := kv.Get(ctx, "a"); !ok || string(v) != "hi" {
		t.Fatalf("got %q %v", v, ok)
	}
	must(t, kv.Update(ctx, "a", func(_ []byte, _ bool) ([]byte, error) { return nil, nil }, 0))
	if _, ok, _ := kv.Get(ctx, "a"); ok {
		t.Fatal("nil result should delete")
	}
}

func TestRedisScan(t *testing.T) {
	kv, _ := store.NewBackend("redis", testConf(t))
	ctx := context.Background()
	must(t, kv.Update(ctx, "todo\x1fa", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, 0))
	must(t, kv.Update(ctx, "todo\x1fb", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, 0))
	must(t, kv.Update(ctx, "res\x1fc", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, 0))
	keys, err := kv.Scan(ctx, "todo\x1f")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2, got %d: %v", len(keys), keys)
	}
}

// TestRedisWrapRegistered:宿主已持有 goredis 客户端时经 Wrap 注册,
// client: <name> 引用后与直连后端等价——读写同一 redis、prefix 照拼。
func TestRedisWrapRegistered(t *testing.T) {
	conf := testConf(t) // 直连探活,不可达即跳过
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379", DB: 12})
	t.Cleanup(func() { rdb.Close() })
	redisconn.RegisterClient("corp-kv-live", redisconn.Wrap(rdb))

	kv, err := store.NewBackend("redis", map[string]any{"client": "corp-kv-live", "prefix": "akkv:"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must(t, kv.Update(ctx, "reg", func(_ []byte, _ bool) ([]byte, error) { return []byte("v"), nil }, 0))
	if v, ok, _ := kv.Get(ctx, "reg"); !ok || string(v) != "v" {
		t.Fatalf("roundtrip via wrapped client: %q %v", v, ok)
	}
	// 与直连后端同 prefix 互视:注册客户端不是隔离世界,只是连接来源不同
	direct, _ := store.NewBackend("redis", conf)
	if v, ok, _ := direct.Get(ctx, "reg"); !ok || string(v) != "v" {
		t.Fatalf("direct backend should see the same key: %q %v", v, ok)
	}
}

// TestRedisThirdPartyClient:第三方自有 Client 实现(以 redisconntest
// 内存实现代表)驱动 redis 后端全功能——无 redis server 也能跑,
// 证明后端对客户端具体类型零依赖。
func TestRedisThirdPartyClient(t *testing.T) {
	redisconn.RegisterClient("corp-kv-fake", redisconntest.New())
	kv, err := store.NewBackend("redis", map[string]any{"client": "corp-kv-fake", "prefix": "p:"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must(t, kv.Update(ctx, "a", func(_ []byte, _ bool) ([]byte, error) { return []byte("hi"), nil }, 0))
	if v, ok, _ := kv.Get(ctx, "a"); !ok || string(v) != "hi" {
		t.Fatalf("got %q %v", v, ok)
	}
	must(t, kv.Update(ctx, "b", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, 0))
	keys, _ := kv.Scan(ctx, "")
	if len(keys) != 2 {
		t.Fatalf("scan want 2, got %v", keys)
	}
	must(t, kv.Delete(ctx, "a"))
	if _, ok, _ := kv.Get(ctx, "a"); ok {
		t.Fatal("delete failed")
	}
}

// TestRedisAtomicUpdate 是 redis 后端的 #1 验收:WATCH/MULTI 乐观锁下
// 多 goroutine 并发自增同键无丢更新(裸 GET+SET 会因竞态失败)。
func TestRedisAtomicUpdate(t *testing.T) {
	kv, _ := store.NewBackend("redis", testConf(t))
	ctx := context.Background()
	const goroutines, iters = 16, 100

	incr := func(old []byte, ok bool) ([]byte, error) {
		var n uint64
		if ok {
			n = binary.LittleEndian.Uint64(old)
		}
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, n+1)
		return buf, nil
	}
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if err := kv.Update(ctx, "counter", incr, 0); err != nil {
					t.Errorf("update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	v, ok, _ := kv.Get(ctx, "counter")
	if !ok {
		t.Fatal("counter missing")
	}
	if got := binary.LittleEndian.Uint64(v); got != goroutines*iters {
		t.Fatalf("lost updates: got %d want %d", got, goroutines*iters)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
