// Package redisconn 定义 agent-kit 对 redis 的全部诉求(Client 能力面
// 接口),并提供基于官方 go-redis 的实现。三个 redis 后端(store.KV /
// session / memory)只消费本接口,不触碰任何客户端具体类型——公司自有
// redis 封装实现 Client 并 RegisterClient 注册,即可整体替换官方客户端
// (配置以 client: <name> 引用)。参考实现见 redisconntest 子包。
package redisconn

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client 是 agent-kit 需要的 redis 能力面:三类数据结构、九个语义操作。
// 接口定义在语义层而非命令层——Update 要求的是"原子读改写"这一契约,
// 官方实现用 WATCH/MULTI 乐观锁,第三方可用 Lua 脚本/CAS/自有事务,
// 手段不限。键由调用方拼好(含 prefix)原样使用,实现不再加工。
type Client interface {
	// —— 字符串键(store.KV 家族:todo/digest/suspend/budget/approval)——

	// Get 读键;不存在时 ok=false 且无错误。
	Get(ctx context.Context, key string) (val []byte, ok bool, err error)
	// Update 原子读改写:mutate 收到当前值(不存在时 ok=false),返回
	// 新值;返回 nil 表示删除该键。并发冲突时实现负责重试,因此 mutate
	// 可能被调用多次,必须无副作用。ttl<=0 表示不过期。
	Update(ctx context.Context, key string, mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error
	// Delete 删键,键不存在不算错。
	Delete(ctx context.Context, key string) error
	// Scan 返回具有给定前缀的全部键(完整键,不剥前缀)。
	Scan(ctx context.Context, prefix string) ([]string, error)

	// —— 列表(session 会话历史:追加日志 + 区间读)——

	// RPush 尾部追加;LRange 语义与 redis 一致(负下标从尾部计,stop=-1 到末尾)。
	RPush(ctx context.Context, key string, vals ...[]byte) error
	LRange(ctx context.Context, key string, start, stop int64) ([][]byte, error)

	// —— 哈希(长期记忆:scope 一键、记忆项为 field)——

	HSet(ctx context.Context, key, field, value string) error
	HGetAll(ctx context.Context, key string) (map[string]string, error)
}

var (
	mu      sync.RWMutex
	clients = map[string]Client{}
)

// RegisterClient 按名注册 Client 实现,配置以 client: <name> 引用。
// 进程启动期(Build 之前)注册,重名 panic(与其他注册表同一纪律)。
// 生命周期归注册方:框架不做连通性自检、不负责关闭。
func RegisterClient(name string, client Client) {
	if name == "" || client == nil {
		panic("redisconn: RegisterClient requires a non-empty name and client")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := clients[name]; dup {
		panic(fmt.Sprintf("redisconn: client %q registered more than once", name))
	}
	clients[name] = client
}

// Dial 解析 redis 客户端。config 两种形态,互斥:
//   - client: <name> —— 引用 RegisterClient 注册的实现(第三方封装);
//   - addr(默认 127.0.0.1:6379)· password · db —— 官方 go-redis 直连
//     单机,ping 自检。已持有 goredis 客户端(含集群/哨兵)的宿主可用
//     Wrap 包一层再注册。
//
// prefix(键前缀,多租隔离)两种形态通用,返回给后端实现拼键。
func Dial(conf map[string]any) (Client, string, error) {
	prefix, _ := conf["prefix"].(string)
	if name, _ := conf["client"].(string); name != "" {
		if conf["addr"] != nil || conf["password"] != nil || conf["db"] != nil {
			return nil, "", fmt.Errorf("redis: client is mutually exclusive with addr/password/db (client references a registered implementation; connection parameters belong to the registrar)")
		}
		mu.RLock()
		c, ok := clients[name]
		mu.RUnlock()
		if !ok {
			return nil, "", fmt.Errorf("redis: client %q is not registered (call redisconn.RegisterClient(%q, <Client implementation>) during host startup; registered: %s)",
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
	return Wrap(rdb), prefix, nil
}

func registeredNames() string {
	mu.RLock()
	defer mu.RUnlock()
	if len(clients) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ---- 官方实现(go-redis)----

// Wrap 把任意 go-redis 客户端(单机/集群/哨兵,满足 UniversalClient)
// 适配为 Client。宿主已持有 goredis 实例时,注册它的最短路径:
//
//	redisconn.RegisterClient("corp", redisconn.Wrap(rdb))
func Wrap(rdb goredis.UniversalClient) Client {
	return &official{rdb: rdb}
}

type official struct {
	rdb goredis.UniversalClient
}

func (o *official) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := o.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// updateRetries 是乐观锁冲突的重试上限;冲突时带抖动退避,避免多写者
// 锁步碰撞而饿死。真实用例(todo/result 按会话分键)几乎无同键并发,
// 上限主要为对抗性场景兜底。
const updateRetries = 200

// Update 以 WATCH/MULTI 乐观锁实现原子读改写契约。
func (o *official) Update(ctx context.Context, key string,
	mutate func(old []byte, ok bool) ([]byte, error), ttl time.Duration) error {

	txf := func(tx *goredis.Tx) error {
		old, err := tx.Get(ctx, key).Bytes()
		ok := true
		if err == goredis.Nil {
			ok, old = false, nil
		} else if err != nil {
			return err
		}
		next, err := mutate(old, ok)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p goredis.Pipeliner) error {
			if next == nil {
				p.Del(ctx, key)
			} else {
				if ttl < 0 {
					ttl = 0 // 负值钳 0:-1 恰是 goredis.KeepTTL,会保留旧过期而非清除
				}
				p.Set(ctx, key, next, ttl)
			}
			return nil
		})
		return err
	}
	for i := 0; i < updateRetries; i++ {
		err := o.rdb.Watch(ctx, txf, key)
		if err == goredis.TxFailedErr { // EXEC 冲突,抖动退避后重试
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
			continue
		}
		return err
	}
	return fmt.Errorf("redis: update %q: too much contention", key)
}

// backoff 返回第 i 次冲突的退避时长:线性增长上限 2ms + 满抖动,
// 打散锁步的写者。
func backoff(i int) time.Duration {
	base := time.Duration(i+1) * 100 * time.Microsecond
	if base > 2*time.Millisecond {
		base = 2 * time.Millisecond
	}
	return base/2 + time.Duration(rand.Int63n(int64(base/2)+1))
}

func (o *official) Delete(ctx context.Context, key string) error {
	return o.rdb.Del(ctx, key).Err()
}

// globEscape 转义 MATCH 模式的 glob 元字符:前缀含 *?[]\ 时按字面匹配。
func globEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`)
	return r.Replace(s)
}

func (o *official) Scan(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	iter := o.rdb.Scan(ctx, 0, globEscape(prefix)+"*", 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

func (o *official) RPush(ctx context.Context, key string, vals ...[]byte) error {
	args := make([]any, 0, len(vals))
	for _, v := range vals {
		args = append(args, v)
	}
	return o.rdb.RPush(ctx, key, args...).Err()
}

func (o *official) LRange(ctx context.Context, key string, start, stop int64) ([][]byte, error) {
	raws, err := o.rdb.LRange(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(raws))
	for _, r := range raws {
		out = append(out, []byte(r))
	}
	return out, nil
}

func (o *official) HSet(ctx context.Context, key, field, value string) error {
	return o.rdb.HSet(ctx, key, field, value).Err()
}

func (o *official) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return o.rdb.HGetAll(ctx, key).Result()
}
