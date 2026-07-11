// Package vector 把向量知识库检索接成一种工具源(source type: vector),
// 与 http/mcp/rpc 并列——RAG 在本框架里就是 tools 层的一种工具,不是
// 独立组件。运行时消费检索结果;离线摄入(文档→切块→embedding)不归
// 框架管,由外部数据 pipeline 或知识库产品负责。
//
// 配置:
//
//	tools:
//	  - name: kb                     # source 名 = 检索能力的 namespace
//	    type: vector
//	    config:
//	      description: "产品文档知识库,含退货/物流政策"  # 决定大脑的调用意愿
//	      tool_name: search_kb       # 可省略,默认 search_knowledge_base
//	      top_k: 5
//	      max_doc_len: 800           # 单文档注入截断,<=0 不截
//	      backend: inmemory          # 缺省内置词法保底;真实向量库经 RegisterBackend
//	      backend_config: {...}      # 真实后端的连接配置
//	      docs: ["退货政策:7天无理由", ...]  # inmemory 后端的语料(demo/测试)
//
// 检索能力自带双形态:component/agent 的工具面引用它 = agentic RAG
// (大脑决定要不要查、查什么);graph skill 的前置步骤引用它 = 经典
// RAG 管线(每轮必查、结果注入),不需要额外机制。
package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/protocol/vectorstore"
)

// 向量库后端协议(BackendFactory/RegisterBackend)已上浮基座 vectorstore 包
// (可扩展接缝);这里只留 source 实现 + 内置词法保底后端的注册。

func init() {
	// 内置 inmemory 词法保底:开发/测试/无外部向量库时可用。
	vectorstore.RegisterBackend("inmemory", func(conf map[string]any) (retriever.Retriever, error) {
		var c struct {
			Docs []string `json:"docs"`
		}
		if err := decode.Config(conf, &c); err != nil {
			return nil, err
		}
		return &inmemoryRetriever{docs: c.Docs}, nil
	})

	// vector 工具源:构造后端 → 包成检索能力。
	source.Register("vector", func(_ context.Context, name string, conf map[string]any) (source.Source, error) {
		var c struct {
			Description   string         `json:"description"`
			ToolName      string         `json:"tool_name"`
			TopK          int            `json:"top_k"`
			MaxDocLen     int            `json:"max_doc_len"`
			Backend       string         `json:"backend"`
			BackendConfig map[string]any `json:"backend_config"`
			Docs          []string       `json:"docs"`
		}
		if err := decode.Config(conf, &c); err != nil {
			return nil, err
		}
		// inmemory 后端的 docs 直接从顶层 config 取(免去嵌套 backend_config)。
		beConf := c.BackendConfig
		if (c.Backend == "" || c.Backend == "inmemory") && beConf == nil {
			beConf = map[string]any{"docs": c.Docs}
		}
		r, err := vectorstore.New(c.Backend, beConf)
		if err != nil {
			return nil, fmt.Errorf("vector source %s: %w", name, err)
		}
		toolName := c.ToolName
		if toolName == "" {
			toolName = "search_knowledge_base"
		}
		desc := c.Description
		if desc == "" {
			desc = "检索知识库,返回与 query 最相关的文档片段。"
		}
		cap := newRetrieverCapability(name, toolName, desc, c.TopK, c.MaxDocLen, r)
		return source.Static(name, cap), nil
	})
}

// newRetrieverCapability 把 eino Retriever 包成一个检索能力(双形态)。
func newRetrieverCapability(ns, toolName, desc string, topK, maxDocLen int, r retriever.Retriever) capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: ns, Name: toolName},
		Description: desc,
		Risk:        capability.RiskReadonly, // 只读检索
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "检索查询语句", Required: true},
		}),
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Query string `json:"query"`
		}
		// 兼容图节点直接传裸字符串。
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Query == "" {
			args.Query = argsJSON
		}
		// 空查询(""/"{}")做相似检索没有意义,兜底命中还会误导模型。
		if q := strings.TrimSpace(args.Query); q == "" || q == "{}" {
			return "invalid arguments: query is required", nil
		}
		var opts []retriever.Option
		if topK > 0 {
			opts = append(opts, retriever.WithTopK(topK))
		}
		docs, err := r.Retrieve(ctx, args.Query, opts...)
		if err != nil {
			return "", fmt.Errorf("retrieve: %w", err)
		}
		if len(docs) == 0 {
			return "no relevant documents found", nil
		}
		var sb strings.Builder
		for i, d := range docs {
			content := d.Content
			if maxDocLen > 0 && len([]rune(content)) > maxDocLen {
				content = string([]rune(content)[:maxDocLen]) + "..."
			}
			fmt.Fprintf(&sb, "[doc %d] %s\n", i+1, content)
		}
		return sb.String(), nil
	})
}

// inmemoryRetriever 是词法保底后端:字符 bigram 重叠打分,对中文友好、
// 零外部依赖。生产语义检索请注册向量后端。
type inmemoryRetriever struct {
	docs []string
}

func (m *inmemoryRetriever) Retrieve(_ context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	o := retriever.GetCommonOptions(&retriever.Options{}, opts...)
	topK := 3
	if o.TopK != nil && *o.TopK > 0 {
		topK = *o.TopK
	}
	qg := bigrams(query)
	if len(qg) == 0 {
		return nil, nil
	}
	type scored struct {
		doc   string
		score int
	}
	var hits []scored
	for _, d := range m.docs {
		if s := overlap(qg, bigrams(d)); s > 0 {
			hits = append(hits, scored{d, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]*schema.Document, 0, len(hits))
	for _, h := range hits {
		out = append(out, &schema.Document{Content: h.doc})
	}
	return out, nil
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
