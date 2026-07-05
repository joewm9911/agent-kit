package file

import (
	"context"
	"testing"
	"time"
)

func TestFileKVRoundtrip(t *testing.T) {
	kv, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "ask\x1f轮次-键/带斜杠" // 键可含任意字符(不可见分隔符、斜杠、中文)

	if err := kv.Update(ctx, key, func([]byte, bool) ([]byte, error) {
		return []byte("值"), nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := kv.Get(ctx, key)
	if err != nil || !ok || string(v) != "值" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	keys, err := kv.Scan(ctx, "ask\x1f")
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("scan: %v %v", keys, err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := kv.Get(ctx, key); ok {
		t.Fatal("should be deleted")
	}
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatal("double delete must not error")
	}
}

func TestFileKVPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	a, _ := New(dir)
	if err := a.Update(ctx, "k", func([]byte, bool) ([]byte, error) {
		return []byte("v"), nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	b, _ := New(dir) // “进程重启”:仅共享磁盘
	v, ok, err := b.Get(ctx, "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("cross-instance read: %q %v %v", v, ok, err)
	}
}

func TestFileKVUpdateReadModifyWriteAndDelete(t *testing.T) {
	kv, _ := New(t.TempDir())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := kv.Update(ctx, "n", func(old []byte, ok bool) ([]byte, error) {
			if !ok {
				return []byte("x"), nil
			}
			return append(old, 'x'), nil
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, _ := kv.Get(ctx, "n"); string(v) != "xxx" {
		t.Fatalf("rmw: %q", v)
	}
	// mutate 返回 nil = 删除
	if err := kv.Update(ctx, "n", func([]byte, bool) ([]byte, error) {
		return nil, nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := kv.Get(ctx, "n"); ok {
		t.Fatal("nil mutate should delete")
	}
}

func TestFileKVTTLExpiry(t *testing.T) {
	kv, _ := New(t.TempDir())
	ctx := context.Background()
	if err := kv.Update(ctx, "tmp", func([]byte, bool) ([]byte, error) {
		return []byte("v"), nil
	}, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok, _ := kv.Get(ctx, "tmp"); ok {
		t.Fatal("expired entry should be gone")
	}
	if keys, _ := kv.Scan(ctx, ""); len(keys) != 0 {
		t.Fatalf("scan must skip expired: %v", keys)
	}
}
