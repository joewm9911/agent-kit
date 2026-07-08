package redisconn_test

import (
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/joewm9911/agent-kit/impl/utils/redisconn"
	"github.com/joewm9911/agent-kit/impl/utils/redisconn/redisconntest"
)

// TestRegisterClientResolve:注册的 Client 实现按名解析,返回同一实例。
func TestRegisterClientResolve(t *testing.T) {
	c := redisconntest.New()
	redisconn.RegisterClient("corp-test", c)

	got, prefix, err := redisconn.Dial(map[string]any{"client": "corp-test", "prefix": "t:"})
	if err != nil {
		t.Fatalf("dial registered client: %v", err)
	}
	if got != c || prefix != "t:" {
		t.Fatalf("should return the registered instance with prefix, got %v %q", got, prefix)
	}
}

// TestRegisterClientNoPing:注册路径不做连通性自检(健康归注册方)——
// Wrap 一个指向不可达地址的 goredis 客户端,Dial 仍成功。
func TestRegisterClientNoPing(t *testing.T) {
	redisconn.RegisterClient("corp-unreachable",
		redisconn.Wrap(goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})))
	if _, _, err := redisconn.Dial(map[string]any{"client": "corp-unreachable"}); err != nil {
		t.Fatalf("registered client must not be pinged: %v", err)
	}
}

// TestRegisterClientFailFast:未注册按名引用、client 与直连参数混写、
// 重复注册,三种违规全部 fail fast 且报错指路。
func TestRegisterClientFailFast(t *testing.T) {
	if _, _, err := redisconn.Dial(map[string]any{"client": "nope"}); err == nil ||
		!strings.Contains(err.Error(), "RegisterClient") {
		t.Fatalf("unknown client must fail with hint, got %v", err)
	}
	if _, _, err := redisconn.Dial(map[string]any{"client": "nope", "addr": "x:1"}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("client+addr must fail, got %v", err)
	}

	redisconn.RegisterClient("dup-test", redisconntest.New())
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	redisconn.RegisterClient("dup-test", redisconntest.New())
}
