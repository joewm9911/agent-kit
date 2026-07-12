package redisconn

// 契约自检套件自身的验证:官方 go-redis 实现必须全绿(有本地 redis 时)。

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestVerifyOfficialClient(t *testing.T) {
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("no local redis:", err)
	}
	if err := Verify(context.Background(), Wrap(rdb)); err != nil {
		t.Fatalf("official client must pass conformance: %v", err)
	}
}
