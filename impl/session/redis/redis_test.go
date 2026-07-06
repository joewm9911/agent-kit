package redis

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/protocol/session"
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
