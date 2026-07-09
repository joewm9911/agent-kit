package bigram

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestSearch(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("我们聊聊竞品定价策略的问题"),
		schema.AssistantMessage("Notion 的定价是每人每月 10 美元", nil),
		schema.UserMessage("今天天气不错"),
		schema.SystemMessage("竞品定价 system 消息不应命中"),
	}
	hits := Search(msgs, "竞品的定价", 2)
	if len(hits) == 0 {
		t.Fatal("expect relevant hits")
	}
	for _, h := range hits {
		if strings.Contains(h, "天气") || strings.Contains(h, "system 消息") {
			t.Fatalf("irrelevant/system message recalled: %s", h)
		}
	}
}

// TestRecencyWeighting(U1.2):两条词法命中相当的消息,更靠后(更新)的排前。
func TestRecencyWeighting(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("订单 O-1042 的状态查询"), // 旧
		schema.UserMessage("闲聊一句无关内容占位"),
		schema.UserMessage("订单 O-1042 的状态怎样"), // 新,与旧几乎同样命中
	}
	hits := Search(msgs, "订单 O-1042 状态", 3)
	if len(hits) < 2 {
		t.Fatalf("expect both order messages, got %v", hits)
	}
	iNew, iOld := indexOfContaining(hits, "怎样"), indexOfContaining(hits, "查询")
	if iNew < 0 || iOld < 0 || iNew >= iOld {
		t.Fatalf("recency should rank the newer hit first: %v", hits)
	}
}

// TestSnippetAroundMatch(U1.4a):匹配点在长消息中间时,返回的片段应含匹配
// 内容,而非只截前缀。
func TestSnippetAroundMatch(t *testing.T) {
	long := strings.Repeat("这是一段与查询无关的开场铺垫叙述。", 12) +
		"P103 当前库存 42 件,需要补货。" +
		strings.Repeat("后面又是一堆无关的收尾话术填充。", 12)
	msgs := []*schema.Message{schema.AssistantMessage(long, nil)}
	hits := Search(msgs, "P103 库存 42", 1)
	if len(hits) != 1 {
		t.Fatalf("expect one hit, got %v", hits)
	}
	if !strings.Contains(hits[0], "库存 42") {
		t.Fatalf("snippet must contain the matched region, got: %s", hits[0])
	}
}

func indexOfContaining(hits []string, sub string) int {
	for i, h := range hits {
		if strings.Contains(h, sub) {
			return i
		}
	}
	return -1
}
