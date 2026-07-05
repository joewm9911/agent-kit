// Package memory 提供长期记忆:以 memory_save / memory_search 能力
// 暴露给模型,由大脑决定何时记、何时查——这是"模型自主"的部分。
// (会话级短期记忆见 session 包,由运行时自动织入,模型无感知。)
//
// 作用域(scope)是记忆的归属维度,多用户场景的隔离基础:
//   - user:<id>  用户私有记忆(偏好、个人事实),by 终端用户隔离;
//   - shared     域共享知识(SOP、流程),跨用户可见;
//   - session:<id> 会话临时记忆(少用)。
//
// 读写不对称是有意的:用户面 agent 的对话写入只落用户桶(写收窄),
// 召回同时覆盖用户桶与共享池(读放开)——共享知识对所有用户可见,
// 但对话里的模型碰不到共享池的写入权。"记忆归谁"由框架包装层按
// 配置施加,模型无感知;往共享池写必须是配置显式授予,不是模型
// 运行时自选(治理归部署方,库不给自己放权)。
//
// Store 后端可替换为 Redis / 向量库;scope 在向量后端对应 metadata 过滤。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
)

// SharedScope 是域共享知识的作用域名。
const SharedScope = "shared"

// Store 是长期记忆的最小契约。scope 是归属维度(user:<id> / shared /
// session:<id>);Search 在给定的一组 scope 内检索。实现可以是关键词
// 匹配,也可以是向量检索(scope → metadata filter)。
type Store interface {
	Put(ctx context.Context, scope, key, value string) error
	Search(ctx context.Context, scopes []string, query string, limit int) (map[string]string, error)
}

// Factory 按配置构造长期记忆后端。
type Factory func(conf map[string]any) (Store, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册后端类型(redis/向量库/自定义),实现方空导入即可在
// 配置里以 memory.store 引用(或 cap://store/memory/<name>)。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("memory: kv type %q already registered", typ))
	}
	factories[typ] = f
}

// New 按类型构造长期记忆后端,空类型默认 inmemory。后端未注册时 fail-fast。
func New(typ string, conf map[string]any) (Store, error) {
	if typ == "" {
		typ = "inmemory"
	}
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		names := make([]string, 0, len(factories))
		for k := range factories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("memory: unknown store type %q; blank-import a backend (e.g. agent-kit/impl/memory/inmemory) or agent-kit/std. registered: %v", typ, names)
	}
	return f(conf)
}

// UserScope 返回终端用户的作用域名。
func UserScope(userID string) string { return "user:" + userID }

// ScopeConfig 是记忆作用域策略:对话写入落到哪一层、召回覆盖哪几层。
type ScopeConfig struct {
	// Write 是对话 memory_save 的落点:user(默认)| shared | session。
	Write string
	// Read 是召回覆盖的作用域:缺省 [user, shared]。
	Read []string
}

// writeScope 解析当前 ctx 下的写入 scope;user 写入但无用户身份时
// 返回错误(fail fast,不静默落进共享池)。
func (c ScopeConfig) writeScope(ctx context.Context) (string, error) {
	switch c.Write {
	case SharedScope:
		return SharedScope, nil
	case "session":
		if s := runctx.Session(ctx); s != "" {
			return "session:" + s, nil
		}
		return "", fmt.Errorf("会话记忆需要会话身份,当前缺失")
	default: // "" | "user"
		if u := runctx.User(ctx); u != "" {
			return UserScope(u), nil
		}
		return "", fmt.Errorf("用户记忆需要终端用户身份,当前通道未提供")
	}
}

// ReadScopes 解析当前 ctx 下召回覆盖的 scope 列表(缺省 user+shared;
// 无用户身份时用户桶自动略过,共享池仍可读)。
func (c ScopeConfig) ReadScopes(ctx context.Context) []string {
	want := c.Read
	if len(want) == 0 {
		want = []string{"user", SharedScope}
	}
	var out []string
	for _, s := range want {
		switch s {
		case "user":
			if u := runctx.User(ctx); u != "" {
				out = append(out, UserScope(u))
			}
		case "session":
			if sess := runctx.Session(ctx); sess != "" {
				out = append(out, "session:"+sess)
			}
		default: // shared 或具体 scope 名
			out = append(out, s)
		}
	}
	return out
}

// AsCapabilities 把长期记忆包装成 memory_save / memory_search 两个能力,
// 按 scope 策略施加归属:save 写入 write scope(无身份 fail fast),
// search 覆盖 read scopes。scope 由框架注入,模型无感知。
// save 是改动性操作但风险可控,标记 readonly 以免每次记录都触发审批;
// 接入敏感存储时可自行用 loop.GateApproval 收紧。
func AsCapabilities(kv Store, scope ScopeConfig) []capability.Capability {
	save := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "memory_save"},
		Description: "保存一条长期记忆。当用户告知偏好、事实或值得跨会话记住的信息时调用。",
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"key":   {Type: schema.String, Desc: "记忆的简短标题(名词短语,便于日后检索)", Required: true},
			"value": {Type: schema.String, Desc: "记忆内容(自包含,不要指代上文)", Required: true},
		}),
	}, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct{ Key, Value string }
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", err
		}
		sc, err := scope.writeScope(ctx)
		if err != nil {
			// 以工具结果回传,让大脑向用户说明,不中断循环。
			return "未能保存记忆:" + err.Error(), nil
		}
		if err := kv.Put(ctx, sc, args.Key, args.Value); err != nil {
			return "", err
		}
		return "saved", nil
	})

	search := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "memory_search"},
		Description: "按关键词检索长期记忆。回答依赖用户历史偏好或既往事实时先调用。",
		Params:      capability.SingleParam("query", "检索关键词"),
	}, func(ctx context.Context, argsJSON string) (string, error) {
		query := capability.ParseSingle(argsJSON, "query")
		hits, err := kv.Search(ctx, scope.ReadScopes(ctx), query, 5)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "no memory found", nil
		}
		var sb strings.Builder
		for k, v := range hits {
			fmt.Fprintf(&sb, "- %s: %s\n", k, v)
		}
		return sb.String(), nil
	})

	return []capability.Capability{save, search}
}
