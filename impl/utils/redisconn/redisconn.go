// Package redisconn 是 redis 后端(store.KV / session / memory)共享的连接件:
// 从配置建客户端并连通性自检。普通共享包(不用 internal),供 impl/ 下各
// redis 实现复用同一份 Dial。
package redisconn

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// Dial 从配置构造 redis 客户端并连通性自检。config:
// addr(默认 127.0.0.1:6379)· password · db · prefix(键前缀,多租隔离)。
// 返回客户端与 prefix(供实现拼键)。
func Dial(conf map[string]any) (*goredis.Client, string, error) {
	addr, _ := conf["addr"].(string)
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password, _ := conf["password"].(string)
	prefix, _ := conf["prefix"].(string)
	db := 0
	switch v := conf["db"].(type) {
	case int:
		db = v
	case float64:
		db = int(v)
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, Password: password, DB: db})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, "", fmt.Errorf("redis: ping %s: %w", addr, err)
	}
	return rdb, prefix, nil
}
