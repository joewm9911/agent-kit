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
//
// Search 返回按相关度排好序的命中,scopes 顺序即优先级(先 user 后
// shared:个人事实压过域共识,同键不同值时不丢失、不覆盖)。旧契约
// map[string]string 三宗罪:无序(prompt cache 敌对)、跨 scope 同键
// 覆盖(方向还反了,shared 压 user)、超 limit 时随机子集(eval 不可
// 复现)——pre-1.0 硬切为 []Hit。
type Store interface {
	Put(ctx context.Context, scope, key, value string) error
	Search(ctx context.Context, scopes []string, query string, limit int) ([]Hit, error)
}

// Hit 是一次检索命中。Score 只在同一后端内可比。
type Hit struct {
	Scope string
	Key   string
	Value string
	Score float64
}

// ScanBucket 是关键词后端(inmemory/redis)共用的打分内核:查询分词后
// 对 key/value 做子串命中(key 权重更高),再叠加字符 bigram Jaccard
// 兜底(容错分词打不中的黏连中文)。返回 Score>0 的命中,未排序。
func ScanBucket(scope string, bucket map[string]string, query string) []Hit {
	tokens := strings.Fields(strings.ToLower(query))
	qgrams := bigrams(query)
	var hits []Hit
	for k, v := range bucket {
		lk, lv := strings.ToLower(k), strings.ToLower(v)
		var score float64
		for _, t := range tokens {
			if strings.Contains(lk, t) {
				score += 2
			} else if strings.Contains(lv, t) {
				score += 1
			}
		}
		if j := jaccard(qgrams, bigrams(k+" "+v)); j > 0 {
			score += j
		}
		if score > 0 {
			hits = append(hits, Hit{Scope: scope, Key: k, Value: v, Score: score})
		}
	}
	return hits
}

// SortHits 按 Score 降序、Key 升序稳定排序(确定序,eval 可复现)。
func SortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Key < hits[j].Key
	})
}

func bigrams(s string) map[string]struct{} {
	r := []rune(strings.ToLower(strings.Join(strings.Fields(s), "")))
	out := map[string]struct{}{}
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for g := range a {
		if _, ok := b[g]; ok {
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(n) / float64(len(a)+len(b)-n)
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
	var names []string
	if !ok { // 名字列表在锁内抄出:错误路径在锁外遍历注册表是数据竞争
		for k := range factories {
			names = append(names, k)
		}
	}
	facMu.RUnlock()
	if !ok {
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
		return "", fmt.Errorf("session memory requires a session identity, which is currently missing")
	default: // "" | "user"
		if u := runctx.User(ctx); u != "" {
			return UserScope(u), nil
		}
		return "", fmt.Errorf("user memory requires an end-user identity, which this channel did not provide")
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
		Description: "Save a long-term memory. Call when the user states a preference, a fact, or information worth remembering across sessions.",
		Risk:        capability.RiskReadonly, // 写的是 agent 自身记忆,不是外部世界;误存可覆盖,不过审批
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"key":   {Type: schema.String, Desc: "A short title for the memory (a noun phrase, for easy later retrieval)", Required: true},
			"value": {Type: schema.String, Desc: "The memory content (self-contained, do not refer to earlier context)", Required: true},
		}),
	}, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct{ Key, Value string }
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", err
		}
		sc, err := scope.writeScope(ctx)
		if err != nil {
			// 以工具结果回传,让大脑向用户说明,不中断循环。
			return "failed to save memory: " + err.Error(), nil
		}
		if err := kv.Put(ctx, sc, args.Key, args.Value); err != nil {
			return "", err
		}
		return "saved", nil
	})

	search := capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "memory_search"},
		Description: "Search long-term memory by keyword. Call first when an answer depends on the user's past preferences or prior facts.",
		Risk:        capability.RiskReadonly,
		Params:      capability.SingleParam("query", "Search keywords"),
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
		for _, h := range hits {
			fmt.Fprintf(&sb, "- %s: %s\n", h.Key, h.Value)
		}
		return sb.String(), nil
	})

	return []capability.Capability{save, search}
}
