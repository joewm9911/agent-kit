package session

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Retriever 是窗外会话召回的检索策略契约:从历史消息中找出与本轮
// 输入相关的片段。查询构造(要不要扩展/改写输入)也是实现的职责。
//
// 这是 Ring 1 扩展点:检索是"同一契约、多种实现、坏了只降质量"的
// 纯函数,语义召回(向量/rerank)注册后即可在配置里以名字引用;
// 织入视图的重建(摘要/锚定/窗口)则是 Ring 0 不变量,不在此开放。
// 内置词法保底 bigram 在 impl/session/bigram。
type Retriever interface {
	Retrieve(ctx context.Context, history []*schema.Message, input string, topK int) []string
}

// RetrieverFactory 按配置构造检索器。
type RetrieverFactory func(conf map[string]any) (Retriever, error)

var (
	retrMu        sync.RWMutex
	retrFactories = map[string]RetrieverFactory{}
)

// RegisterRetriever 注册检索策略(bigram/向量/rerank/自定义):实现方 init
// 自注册,空导入即可在配置里以 retriever: <name> 引用。
func RegisterRetriever(name string, f RetrieverFactory) {
	retrMu.Lock()
	defer retrMu.Unlock()
	if _, ok := retrFactories[name]; ok {
		panic(fmt.Sprintf("session: retriever %q already registered", name))
	}
	retrFactories[name] = f
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
		return nil, fmt.Errorf("session: unknown retriever %q; blank-import a retriever (e.g. agent-kit/impl/session/bigram) or agent-kit/std. registered: %v", name, names)
	}
	return f(conf)
}
