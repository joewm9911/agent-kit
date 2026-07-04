package store

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func TestInMemoryGetPutDelete(t *testing.T) {
	kv := NewInMemory()
	ctx := context.Background()

	if _, ok, _ := kv.Get(ctx, "a"); ok {
		t.Fatal("empty store should miss")
	}
	set(t, kv, "a", []byte("hello"))
	if v, ok, _ := kv.Get(ctx, "a"); !ok || string(v) != "hello" {
		t.Fatalf("got %q ok=%v", v, ok)
	}
	// 返回副本:改动不回写内部
	v, _, _ := kv.Get(ctx, "a")
	v[0] = 'H'
	if again, _, _ := kv.Get(ctx, "a"); string(again) != "hello" {
		t.Fatalf("internal state mutated: %q", again)
	}
	if err := kv.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := kv.Get(ctx, "a"); ok {
		t.Fatal("delete should remove")
	}
}

func TestInMemoryUpdateDeleteOnNil(t *testing.T) {
	kv := NewInMemory()
	ctx := context.Background()
	set(t, kv, "k", []byte("v"))
	// mutate 返回 nil = 删除
	if err := kv.Update(ctx, "k", func(_ []byte, _ bool) ([]byte, error) { return nil, nil }, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := kv.Get(ctx, "k"); ok {
		t.Fatal("nil mutate result should delete")
	}
}

func TestInMemoryTTL(t *testing.T) {
	kv := NewInMemory()
	ctx := context.Background()
	if err := kv.Update(ctx, "k", func(_ []byte, _ bool) ([]byte, error) { return []byte("v"), nil }, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := kv.Get(ctx, "k"); !ok {
		t.Fatal("should be live before expiry")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok, _ := kv.Get(ctx, "k"); ok {
		t.Fatal("should expire after ttl")
	}
}

func TestInMemoryScan(t *testing.T) {
	kv := NewInMemory()
	ctx := context.Background()
	set(t, kv, "todo\x1fa\x1f1", []byte("x"))
	set(t, kv, "todo\x1fa\x1f2", []byte("x"))
	set(t, kv, "sess\x1fb", []byte("x"))
	keys, err := kv.Scan(ctx, "todo\x1f")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 todo keys, got %d: %v", len(keys), keys)
	}
}

// TestInMemoryAtomicUpdate 是 #1 的验收核心:多副本并发读改写无丢更新。
// N 个 goroutine 各自对同一键 Update 自增 M 次,期望最终计数 = N*M。
// 若 Update 不是原子的(裸 Get+Put),这里会因竞态丢更新而失败。
func TestInMemoryAtomicUpdate(t *testing.T) {
	kv := NewInMemory()
	ctx := context.Background()
	const goroutines, iters = 32, 500

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
	got := binary.LittleEndian.Uint64(v)
	if want := uint64(goroutines * iters); got != want {
		t.Fatalf("lost updates: got %d want %d", got, want)
	}
}

func set(t *testing.T, kv KV, key string, val []byte) {
	t.Helper()
	if err := kv.Update(context.Background(), key, func(_ []byte, _ bool) ([]byte, error) { return val, nil }, 0); err != nil {
		t.Fatal(err)
	}
}
