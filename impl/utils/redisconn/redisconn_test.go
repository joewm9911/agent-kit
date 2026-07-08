package redisconn

import (
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

// TestRegisterClientResolve:注册的客户端按名解析,返回同一实例且不 ping
// (构造指向不可达地址的客户端,Dial 仍应成功——健康归注册方)。
func TestRegisterClientResolve(t *testing.T) {
	c := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	RegisterClient("corp-test", c)

	got, prefix, err := Dial(map[string]any{"client": "corp-test", "prefix": "t:"})
	if err != nil {
		t.Fatalf("dial registered client: %v", err)
	}
	if got != goredis.UniversalClient(c) || prefix != "t:" {
		t.Fatalf("should return the registered instance with prefix, got %v %q", got, prefix)
	}
}

// TestRegisterClientFailFast:未注册按名引用、client 与直连参数混写、
// 重复注册,三种违规全部 fail fast 且报错指路。
func TestRegisterClientFailFast(t *testing.T) {
	if _, _, err := Dial(map[string]any{"client": "nope"}); err == nil ||
		!strings.Contains(err.Error(), "RegisterClient") {
		t.Fatalf("unknown client must fail with hint, got %v", err)
	}
	if _, _, err := Dial(map[string]any{"client": "nope", "addr": "x:1"}); err == nil ||
		!strings.Contains(err.Error(), "互斥") {
		t.Fatalf("client+addr must fail, got %v", err)
	}

	c := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	RegisterClient("dup-test", c)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	RegisterClient("dup-test", c)
}
