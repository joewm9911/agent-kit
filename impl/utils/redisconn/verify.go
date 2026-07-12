package redisconn

// verify.go:第三方 Client 实现的契约自检。框架把原子读改写、"不存在
// 与错误可区分"等语义契约完全委托给 Client 实现——公司自有封装若有
// 违约(错误映射成不存在、Update 非原子、TTL 忽略、二进制键截断),
// 症状会在很远的地方爆发(read_result 读不到、审批决策丢失、预算不减),
// 且极难归因。宿主注册后调用一次 Verify 即可在启动期暴露全部违约点:
//
//	redisconn.RegisterClient("corp", myClient)
//	if err := redisconn.Verify(ctx, myClient); err != nil {
//		log.Fatal("redis client 契约违约: ", err)
//	}
//
// 键统一带 "agentkit-verify:" 前缀并在结束时清理;对生产库无残留。

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Verify 对 Client 实现跑契约自检,返回第一个违约点(nil = 全部通过)。
// 会真实读写少量键(前缀 agentkit-verify:),结束时清理;TTL 用例含一次
// ~1.2s 的等待。
func Verify(ctx context.Context, c Client) error {
	const p = "agentkit-verify:"
	cleanup := func() {
		for _, k := range []string{p + "miss", p + "bin\x1fr1", p + "cnt", p + "ttl", p + "del", p + "scan\x1fa", p + "scan\x1fb"} {
			_ = c.Delete(ctx, k)
		}
	}
	cleanup()
	defer cleanup()

	// 1. Get 不存在:必须 (nil,false,nil)——错误映射成不存在是最恶性的
	// 违约(后端抖动时有效数据被报"不存在",调用方永久放弃)。
	if v, ok, err := c.Get(ctx, p+"miss"); err != nil {
		return fmt.Errorf("Get(missing): want (nil,false,nil), got err=%v (backend unreachable? fix connectivity before verifying)", err)
	} else if ok || v != nil {
		return fmt.Errorf("Get(missing): want ok=false, got ok=%v v=%q", ok, v)
	}

	// 2. 二进制键(框架键含 \x1f 分隔符)+ 值往返。
	key := p + "bin\x1fr1"
	want := []byte("原文\x00binary✓")
	if err := c.Update(ctx, key, func(_ []byte, ok bool) ([]byte, error) {
		if ok {
			return nil, fmt.Errorf("fresh key reported existing")
		}
		return want, nil
	}, 0); err != nil {
		return fmt.Errorf("Update(create binary key): %v", err)
	}
	if v, ok, err := c.Get(ctx, key); err != nil || !ok || !bytes.Equal(v, want) {
		return fmt.Errorf("Get(binary key \\x1f): want roundtrip, got ok=%v err=%v v=%q (a client that mangles non-printable keys breaks every framework key)", ok, err, v)
	}

	// 3. Update 原子性:20 并发 × 10 自增必须恰得 200(WATCH/Lua 级契约;
	// Get-then-Set 实现在这里丢更新)。
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := c.Update(ctx, p+"cnt", func(old []byte, ok bool) ([]byte, error) {
					n := 0
					if ok {
						n, _ = strconv.Atoi(string(old))
					}
					return []byte(strconv.Itoa(n + 1)), nil
				}, 0); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return fmt.Errorf("Update(concurrent): %v", err)
	default:
	}
	if v, _, _ := c.Get(ctx, p+"cnt"); string(v) != "200" {
		return fmt.Errorf("Update(atomicity): 20 writers x 10 increments want 200, got %q (read-modify-write is not atomic; lost updates corrupt budgets/approvals/seq counters)", v)
	}

	// 4. Update 返回 nil = 删除。
	if err := c.Update(ctx, p+"del", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, 0); err != nil {
		return fmt.Errorf("Update(set for delete): %v", err)
	}
	if err := c.Update(ctx, p+"del", func(_ []byte, _ bool) ([]byte, error) { return nil, nil }, 0); err != nil {
		return fmt.Errorf("Update(nil=delete): %v", err)
	}
	if _, ok, _ := c.Get(ctx, p+"del"); ok {
		return fmt.Errorf("Update(nil=delete): key still exists (suspend turn claims rely on delete-on-nil)")
	}

	// 5. TTL 生效(1s 过期,容忍到 3s)。忽略 TTL 会让挂起/结果暂存永久堆积。
	if err := c.Update(ctx, p+"ttl", func(_ []byte, _ bool) ([]byte, error) { return []byte("x"), nil }, time.Second); err != nil {
		return fmt.Errorf("Update(ttl): %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok, _ := c.Get(ctx, p+"ttl"); !ok {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("TTL: key still alive 3s after 1s ttl (ttl ignored; suspend/result records will accumulate forever)")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 6. Scan 前缀语义:命中带前缀的、不带的不进来。
	for _, k := range []string{p + "scan\x1fa", p + "scan\x1fb"} {
		if err := c.Update(ctx, k, func(_ []byte, _ bool) ([]byte, error) { return []byte("1"), nil }, 0); err != nil {
			return fmt.Errorf("Update(scan setup): %v", err)
		}
	}
	keys, err := c.Scan(ctx, p+"scan\x1f")
	if err != nil {
		return fmt.Errorf("Scan: %v", err)
	}
	if len(keys) != 2 {
		return fmt.Errorf("Scan(prefix): want exactly 2 keys, got %d %v (read_result 的可取回清单、按 scope 清理都依赖它)", len(keys), keys)
	}
	return nil
}
