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
