package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Retriever 是窗外会话召回的检索策略契约:从历史消息中找出与本轮
// 输入相关的片段。查询构造(要不要扩展/改写输入)也是实现的职责。
//
// 这是 Ring 1 扩展点:检索是"同一契约、多种实现、坏了只降质量"的
// 纯函数,语义召回(向量/rerank)注册后即可在配置里以名字引用;
// 织入视图的重建(摘要/锚定/窗口)则是 Ring 0 不变量,不在此开放。
type Retriever interface {
	Retrieve(ctx context.Context, history []*schema.Message, input string, topK int) []string
}

// RetrieverFactory 按配置构造检索器。
type RetrieverFactory func(conf map[string]any) (Retriever, error)

var (
	retrMu        sync.RWMutex
	retrFactories = map[string]RetrieverFactory{}
)

// RegisterRetriever 注册检索策略(向量/rerank/自定义),与 store 的
// 工厂机制同构:实现方空导入即可在配置里以 retriever: <name> 引用。
func RegisterRetriever(name string, f RetrieverFactory) {
	retrMu.Lock()
	defer retrMu.Unlock()
	if _, ok := retrFactories[name]; ok {
		panic(fmt.Sprintf("session: retriever %q already registered", name))
	}
	retrFactories[name] = f
}

func init() {
	RegisterRetriever("bigram", func(_ map[string]any) (Retriever, error) {
		return bigramRetriever{}, nil
	})
}

// NewRetriever 按名字构造检索器,空名默认 bigram(内置词法保底)。
func NewRetriever(name string, conf map[string]any) (Retriever, error) {
	if name == "" {
		name = "bigram"
	}
	retrMu.RLock()
	f, ok := retrFactories[name]
	retrMu.RUnlock()
	if !ok {
		names := make([]string, 0, len(retrFactories))
		for k := range retrFactories {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("session: unknown retriever %q, registered: %v", name, names)
	}
	return f(conf)
}

// bigramRetriever 是内置的词法检索器(SearchRelevant 的 Retriever 形态)。
type bigramRetriever struct{}

func (bigramRetriever) Retrieve(_ context.Context, history []*schema.Message, input string, topK int) []string {
	return SearchRelevant(history, input, topK)
}

// SearchRelevant 在历史消息中按与 query 的相关度检索,返回格式化片段。
// 打分用字符 bigram 重叠(对中文友好,无需分词),只看 user/assistant
// 消息,命中太弱(<2 个 bigram)不返回。这是无 embedding 依赖的保底
// 实现;语义召回可换向量后端,在长期记忆 KV 层做。
func SearchRelevant(msgs []*schema.Message, query string, topK int) []string {
	if topK <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	qgrams := bigrams(query)
	if len(qgrams) == 0 {
		return nil
	}

	type hit struct {
		score int
		text  string
	}
	var hits []hit
	for _, m := range msgs {
		if m.Role != schema.User && m.Role != schema.Assistant {
			continue
		}
		if m.Content == "" {
			continue
		}
		score := overlap(qgrams, bigrams(m.Content))
		if score < 2 {
			continue
		}
		hits = append(hits, hit{score: score, text: fmt.Sprintf("%s: %s", m.Role, clip(m.Content, 120))})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.text)
	}
	return out
}

func bigrams(s string) map[string]struct{} {
	r := []rune(strings.ToLower(strings.Join(strings.Fields(s), "")))
	out := map[string]struct{}{}
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])] = struct{}{}
	}
	return out
}

func overlap(a, b map[string]struct{}) int {
	n := 0
	for g := range a {
		if _, ok := b[g]; ok {
			n++
		}
	}
	return n
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
