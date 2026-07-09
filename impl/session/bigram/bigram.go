// Package bigram 是 session 的内置词法召回器(retriever: bigram):字符
// bigram 重叠打分,对中文友好、无 embedding 依赖的保底实现。语义召回请
// 注册向量后端。空导入(或经 agent-kit/std)即注册。
//
// 打分与片段(brain-loop U1.2/U1.3/U1.4a):相关度 = Jaccard 归一(消除长
// 消息的原始计数虚高)× 近因因子(对话相关性随距离衰减);命中片段取
// **匹配点周围的窗口**而非消息前缀(前缀常不含匹配到的词,定位对了却答非
// 所问)。retriever_config 可配 snippet_len(片段 rune 上限)与 max_chars
// (召回总字符预算,0 = 不限)。
package bigram

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/protocol/session"
)

func init() {
	session.RegisterRetriever("bigram", func(conf map[string]any) (session.Retriever, error) {
		return retriever{opts: optsFromConf(conf)}, nil
	})
}

// Options 是召回的可配参数。
type Options struct {
	SnippetLen int // 片段 rune 上限(默认 defaultSnippetLen)
	MaxChars   int // 召回总字符预算,0 = 不限
}

const (
	defaultSnippetLen = 160
	minRawOverlap     = 2 // 少于这么多共享 bigram 视为未命中(粗过滤,消噪)
)

func optsFromConf(conf map[string]any) Options {
	o := Options{SnippetLen: defaultSnippetLen}
	if v, ok := conf["snippet_len"]; ok {
		if n := toInt(v); n > 0 {
			o.SnippetLen = n
		}
	}
	if v, ok := conf["max_chars"]; ok {
		o.MaxChars = toInt(v)
	}
	return o
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

type retriever struct{ opts Options }

func (r retriever) Retrieve(_ context.Context, history []*schema.Message, input string, topK int) []string {
	return SearchOpts(history, input, topK, r.opts)
}

// Search 用默认参数检索(包级便捷入口,测试与无配置路径用)。
func Search(msgs []*schema.Message, query string, topK int) []string {
	return SearchOpts(msgs, query, topK, Options{SnippetLen: defaultSnippetLen})
}

// SearchOpts 在历史消息中按相关度检索并返回格式化片段。相关度 = Jaccard ×
// 近因;只看 user/assistant 消息;原始重叠 < minRawOverlap 视为未命中。
func SearchOpts(msgs []*schema.Message, query string, topK int, opts Options) []string {
	if topK <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	qgrams := bigrams(query)
	if len(qgrams) == 0 {
		return nil
	}
	if opts.SnippetLen <= 0 {
		opts.SnippetLen = defaultSnippetLen
	}

	type hit struct {
		score float64
		text  string
	}
	var hits []hit
	n := len(msgs)
	for i, m := range msgs {
		if m.Role != schema.User && m.Role != schema.Assistant {
			continue
		}
		if m.Content == "" {
			continue
		}
		mg := bigrams(m.Content)
		raw := overlap(qgrams, mg)
		if raw < minRawOverlap {
			continue
		}
		// U1.3 Jaccard 归一:raw / |q ∪ m|,消除长消息原始计数虚高。
		jac := float64(raw) / float64(len(qgrams)+len(mg)-raw)
		// U1.2 近因因子:越靠近末尾(越新)权重越高,0.5(最旧)→1.0(最新)。
		rec := 1.0
		if n > 1 {
			rec = 0.5 + 0.5*float64(i)/float64(n-1)
		}
		snippet := snippetAround(m.Content, qgrams, opts.SnippetLen) // U1.4a
		hits = append(hits, hit{score: jac * rec, text: fmt.Sprintf("%s: %s", m.Role, snippet)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]string, 0, len(hits))
	used := 0
	for _, h := range hits {
		if opts.MaxChars > 0 && used+len([]rune(h.text)) > opts.MaxChars && len(out) > 0 {
			break // U1.4c 总量预算:超预算即停(至少留一条)
		}
		out = append(out, h.text)
		used += len([]rune(h.text))
	}
	return out
}

// snippetAround 返回内容中命中 query bigram 最密集区段为中心的窗口(U1.4a),
// 而非前缀——前缀常不含匹配到的词。无命中位置时退回前缀。
func snippetAround(content string, qgrams map[string]struct{}, snippetLen int) string {
	content = strings.Join(strings.Fields(content), " ")
	r := []rune(strings.ToLower(content))
	orig := []rune(content) // 展示用原大小写
	if len(r) <= snippetLen {
		return content
	}
	var matches []int
	for i := 0; i+1 < len(r); i++ {
		if _, ok := qgrams[string(r[i:i+2])]; ok {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return string(orig[:snippetLen]) + "…"
	}
	// 双指针:找覆盖最多命中位置的 snippetLen 窗口,取其左端为中心起点。
	bestStart, bestCount, lo := matches[0], 0, 0
	for hi := 0; hi < len(matches); hi++ {
		for matches[hi]-matches[lo] >= snippetLen {
			lo++
		}
		if hi-lo+1 > bestCount {
			bestCount = hi - lo + 1
			bestStart = matches[lo]
		}
	}
	start := bestStart - snippetLen/4 // 留一点左侧上下文
	if start < 0 {
		start = 0
	}
	end := start + snippetLen
	if end > len(orig) {
		end = len(orig)
		if start = end - snippetLen; start < 0 {
			start = 0
		}
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(orig) {
		suffix = "…"
	}
	return prefix + string(orig[start:end]) + suffix
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
