// Package bigram 是 session 的内置词法召回器(retriever: bigram):字符
// bigram 重叠打分,对中文友好、无 embedding 依赖的保底实现。语义召回请
// 注册向量后端。空导入(或经 agent-kit/std)即注册。
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
	session.RegisterRetriever("bigram", func(_ map[string]any) (session.Retriever, error) {
		return retriever{}, nil
	})
}

type retriever struct{}

func (retriever) Retrieve(_ context.Context, history []*schema.Message, input string, topK int) []string {
	return Search(history, input, topK)
}

// Search 在历史消息中按与 query 的相关度检索,返回格式化片段。打分用字符
// bigram 重叠(对中文友好,无需分词),只看 user/assistant 消息,命中太弱
// (<2 个 bigram)不返回。
func Search(msgs []*schema.Message, query string, topK int) []string {
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
