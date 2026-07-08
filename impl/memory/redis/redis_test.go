package redis

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/impl/utils/redisconn/redisconntest"
	"github.com/joewm9911/agent-kit/protocol/memory"
)

func testConf(t *testing.T) map[string]any {
	t.Helper()
	// db 14(与 impl/session/redis 的 db 15 分开):go test 并行跑各包,
	// 同 db 的 FlushDB 会互相清空。
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379", DB: 14})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis 不可达,跳过: %v", err)
	}
	rdb.FlushDB(context.Background())
	t.Cleanup(func() { rdb.FlushDB(context.Background()); rdb.Close() })
	return map[string]any{"addr": "127.0.0.1:6379", "db": 14, "prefix": "aktest:"}
}

// TestRedisMemory 验证 redis 长期记忆:按 scope 分桶写入、关键词检索限于
// 给定 scopes、limit 生效,以及跨副本(第二个客户端接同一 redis)可读——
// 分布式长期记忆的地基。
func TestRedisMemory(t *testing.T) {
	conf := testConf(t)
	kv, err := memory.New("redis", conf)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must(t, kv.Put(ctx, memory.UserScope("u1"), "预算", "上限 100 万"))
	must(t, kv.Put(ctx, memory.SharedScope, "发布流程", "灰度→观察→全量"))
	must(t, kv.Put(ctx, memory.UserScope("u2"), "预算", "上限 50 万")) // 别的用户,不该串桶

	// 关键词命中,限于 u1 + shared:不含 u2 的记忆
	hits, err := kv.Search(ctx, []string{memory.UserScope("u1"), memory.SharedScope}, "预算", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hits["预算"] != "上限 100 万" {
		t.Fatalf("u1 预算 miss/串桶: %v", hits)
	}
	// scope 边界:只读 shared 时读不到用户桶
	shared, _ := kv.Search(ctx, []string{memory.SharedScope}, "预算", 5)
	if len(shared) != 0 {
		t.Fatalf("shared scope 不该命中用户预算: %v", shared)
	}

	// 跨副本:全新客户端接同一 redis,续读同一记忆
	kv2, _ := memory.New("redis", conf)
	again, _ := kv2.Search(ctx, []string{memory.UserScope("u1")}, "100 万", 5)
	if again["预算"] != "上限 100 万" {
		t.Fatalf("跨副本读长期记忆失败: %v", again)
	}

	// limit 生效
	must(t, kv.Put(ctx, memory.SharedScope, "流程A", "步骤流程"))
	must(t, kv.Put(ctx, memory.SharedScope, "流程B", "步骤流程"))
	capped, _ := kv.Search(ctx, []string{memory.SharedScope}, "流程", 1)
	if len(capped) != 1 {
		t.Fatalf("limit=1 应只返回 1 条,得 %d", len(capped))
	}
}

// TestMemoryThirdPartyClient:第三方 Client 实现驱动长期记忆后端
// (scope 分桶/关键词检索),无 redis server。
func TestMemoryThirdPartyClient(t *testing.T) {
	redisconn.RegisterClient("corp-mem-fake", redisconntest.New())
	kv, err := memory.New("redis", map[string]any{"client": "corp-mem-fake"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must(t, kv.Put(ctx, memory.UserScope("u1"), "预算", "上限 100 万"))
	must(t, kv.Put(ctx, memory.UserScope("u2"), "预算", "上限 50 万"))
	hits, err := kv.Search(ctx, []string{memory.UserScope("u1")}, "预算", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hits["预算"] != "上限 100 万" || len(hits) != 1 {
		t.Fatalf("scope 检索错: %v", hits)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
