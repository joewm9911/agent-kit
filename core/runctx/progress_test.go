package runctx

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestProgressNoSinkZeroCost:未订阅时发射是空操作。
func TestProgressNoSinkZeroCost(t *testing.T) {
	EmitProgress(context.Background(), ProgressEvent{CapKind: "tool", Name: "x"}) // 不 panic 即可
}

// TestProgressAsyncDelivery:事件异步送达,Seq 单调,发射端不阻塞。
func TestProgressAsyncDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []ProgressEvent
	ctx = WithProgress(ctx, func(_ context.Context, ev ProgressEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	for i := 0; i < 5; i++ {
		EmitProgress(ctx, ProgressEvent{CapKind: "tool", Name: "t", Status: "start"})
	}
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 5 })
	mu.Lock()
	defer mu.Unlock()
	for i, ev := range got {
		if ev.Seq != i+1 {
			t.Fatalf("seq[%d] = %d", i, ev.Seq)
		}
	}
}

// TestProgressEmitterNeverBlocks:订阅者卡死时,发射端照样瞬间返回,
// 队列满丢弃(Seq 缺口),主流程不受影响。
func TestProgressEmitterNeverBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	ctx = WithProgress(ctx, func(context.Context, ProgressEvent) { <-block }) // 卡死的订阅者
	start := time.Now()
	for i := 0; i < progressBuffer*3; i++ {
		EmitProgress(ctx, ProgressEvent{CapKind: "tool", Name: "t"})
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("emit blocked: %v", elapsed)
	}
	close(block)
}

// TestProgressSinkPanicIsolated:订阅者 panic 不影响后续投递。
func TestProgressSinkPanicIsolated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var n int
	ctx = WithProgress(ctx, func(_ context.Context, ev ProgressEvent) {
		mu.Lock()
		n++
		mu.Unlock()
		if ev.Seq == 1 {
			panic("boom")
		}
	})
	EmitProgress(ctx, ProgressEvent{Name: "a"})
	EmitProgress(ctx, ProgressEvent{Name: "b"})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return n == 2 })
}

// TestProgressWorkerStopsWithCtx:ctx 结束后 worker 退出,发射不 panic。
func TestProgressWorkerStopsWithCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithProgress(ctx, func(context.Context, ProgressEvent) {})
	cancel()
	time.Sleep(20 * time.Millisecond)
	EmitProgress(ctx, ProgressEvent{Name: "after-cancel"}) // 入队或丢弃,均不得 panic
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
