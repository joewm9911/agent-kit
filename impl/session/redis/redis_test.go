package redis

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/session"
	"github.com/joewm9911/agent-kit/store"
)

// testConf 用独立 db 与随机前缀,测试互不干扰;redis 不可达则跳过。
func testConf(t *testing.T) map[string]any {
	t.Helper()
	conf := map[string]any{"addr": "127.0.0.1:6379", "db": 15, "prefix": "aktest:"}
	rdb, _, err := redisconn.Dial(conf)
	if err != nil {
		t.Skipf("redis 不可达,跳过: %v", err)
	}
	rdb.FlushDB(context.Background())
	t.Cleanup(func() { rdb.FlushDB(context.Background()); rdb.Close() })
	return conf
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

func TestRedisSession(t *testing.T) {
	st, err := session.New("redis", testConf(t), 2) // window=2
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must(t, st.Append(ctx, "s1", schema.UserMessage("一"), schema.UserMessage("二"), schema.UserMessage("三")))

	win, err := st.Load(ctx, "s1") // 窗口裁剪:最近 2 条
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != 2 || win[0].Content != "二" || win[1].Content != "三" {
		t.Fatalf("window wrong: %+v", win)
	}
	full, _ := st.(session.FullLoader).LoadAll(ctx, "s1")
	if len(full) != 3 {
		t.Fatalf("full should be 3, got %d", len(full))
	}
	must(t, st.Clear(ctx, "s1"))
	if all, _ := st.(session.FullLoader).LoadAll(ctx, "s1"); len(all) != 0 {
		t.Fatalf("clear failed: %d", len(all))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
