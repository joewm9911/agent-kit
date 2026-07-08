// Package redisconn 是 redis 后端(store.KV / session / memory)共享的连接件:
// 从配置直连建客户端,或按名引用宿主注册的客户端(公司自有封装)。
// 普通共享包(不用 internal),供 impl/ 下各 redis 实现复用同一份 Dial。
package redisconn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

var (
	mu      sync.RWMutex
	clients = map[string]goredis.UniversalClient{}
)

// RegisterClient 按名注册宿主构造的客户端,配置以 client: <name> 引用。
// 集群/哨兵/TLS/公司 SDK 封装均可,满足 goredis.UniversalClient 即可。
// 进程启动期(Build 之前)注册,重名 panic(与其他注册表同一纪律)。
// 生命周期归宿主:框架不 ping 不 Close,健康自检与关闭由注册方负责。
func RegisterClient(name string, client goredis.UniversalClient) {
	if name == "" || client == nil {
		panic("redisconn: RegisterClient 需要非空 name 与 client")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := clients[name]; dup {
		panic(fmt.Sprintf("redisconn: client %q 重复注册", name))
	}
	clients[name] = client
}

// Dial 解析 redis 客户端。config 两种形态,互斥:
//   - client: <name> —— 引用 RegisterClient 注册的客户端(不 ping,
//     连接参数与健康归注册方);
//   - addr(默认 127.0.0.1:6379)· password · db —— 直连单机,ping 自检。
//
// prefix(键前缀,多租隔离)两种形态通用,返回给实现拼键。
func Dial(conf map[string]any) (goredis.UniversalClient, string, error) {
	prefix, _ := conf["prefix"].(string)
	if name, _ := conf["client"].(string); name != "" {
		if conf["addr"] != nil || conf["password"] != nil || conf["db"] != nil {
			return nil, "", fmt.Errorf("redis: client 与 addr/password/db 互斥(client 引用已注册客户端,连接参数由注册方决定)")
		}
		mu.RLock()
		c, ok := clients[name]
		mu.RUnlock()
		if !ok {
			return nil, "", fmt.Errorf("redis: client %q 未注册(宿主启动期调用 redisconn.RegisterClient(%q, <客户端>);已注册:%s)",
				name, name, registeredNames())
		}
		return c, prefix, nil
	}

	addr, _ := conf["addr"].(string)
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password, _ := conf["password"].(string)
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

func registeredNames() string {
	mu.RLock()
	defer mu.RUnlock()
	if len(clients) == 0 {
		return "(无)"
	}
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
