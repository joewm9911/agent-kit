// Package rag 把 eino Retriever 包装成能力,支持两种使用模式:
//
//   - tool 模式(AsTool):检索作为工具挂给模型,大脑判断"这个问题需不需要
//     查知识库"以及"用什么 query 查",还可以多轮改写 query 重查 ——
//     即 agentic RAG;
//   - node 模式(AsLambda):检索作为图的固定前置节点,每轮必查、结果注入
//     上下文 —— 即经典 RAG 管线,延迟和成本可预期。
package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

// Config 声明一个知识库检索能力。
type Config struct {
	Name        string
	Namespace   string // 所属 source 名,默认 rag
	Description string // 告诉模型这个知识库里有什么,决定大脑的调用意愿
	TopK        int
	MaxDocLen   int // 单文档注入上下文的截断长度,<=0 不截断
}

// New 包装一个 Retriever 为能力。
func New(cfg Config, r retriever.Retriever) capability.Capability {
	if cfg.Name == "" {
		cfg.Name = "search_knowledge_base"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "rag"
	}
	if cfg.Description == "" {
		cfg.Description = "检索知识库,返回与 query 最相关的文档片段。"
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "retriever", Provider: "rag", Namespace: cfg.Namespace, Name: cfg.Name},
		Description: cfg.Description,
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "检索查询语句", Required: true},
		}),
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Query string `json:"query"`
		}
		// 兼容图节点直接传裸字符串的情况。
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Query == "" {
			args.Query = argsJSON
		}
		var opts []retriever.Option
		if cfg.TopK > 0 {
			opts = append(opts, retriever.WithTopK(cfg.TopK))
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
			if cfg.MaxDocLen > 0 && len(content) > cfg.MaxDocLen {
				content = content[:cfg.MaxDocLen] + "..."
			}
			fmt.Fprintf(&sb, "[doc %d] %s\n", i+1, content)
		}
		return sb.String(), nil
	})
}
