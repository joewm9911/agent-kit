package vector

import (
	"context"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/source"
)

// TestVectorSourceRetrieval 验证 vector 工具源:装配为检索能力,
// inmemory 后端按词法相关度返回命中。
func TestVectorSourceRetrieval(t *testing.T) {
	src, err := source.New(context.Background(), "vector", "kb", map[string]any{
		"description": "产品政策知识库",
		"top_k":       2,
		"docs": []any{
			"退货政策:自签收起7天内无理由退货",
			"物流政策:偏远地区加收运费",
			"会员权益:黑卡会员免运费",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := src.Sync(context.Background())
	if err != nil || len(caps) != 1 {
		t.Fatalf("sync: %d caps %v", len(caps), err)
	}
	c := caps[0]

	// Ref:kind=retriever provider=vector namespace=源名,能被 tools/kb/... 匹配
	if ref := c.Meta().Ref.String(); ref != "cap://tool/kb/search_knowledge_base" {
		t.Fatalf("ref = %s", ref)
	}

	out, err := capability.Invoke(context.Background(), c, `{"query":"退货"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "7天内无理由") {
		t.Fatalf("expected retention hit, got %q", out)
	}
	// top_k=2:不该把全部三条都倒出来
	if strings.Count(out, "[doc ") > 2 {
		t.Fatalf("top_k not honored: %q", out)
	}

	// 无命中:明确文案而非空串
	out, _ = capability.Invoke(context.Background(), c, `{"query":"完全无关的宇宙飞船"}`)
	if !strings.Contains(out, "no relevant documents") {
		t.Fatalf("miss case: %q", out)
	}
}

// TestVectorUnknownBackend 验证未注册后端装配期报错。
func TestVectorUnknownBackend(t *testing.T) {
	_, err := source.New(context.Background(), "vector", "kb", map[string]any{
		"backend": "ghost",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("expect unknown backend error, got %v", err)
	}
}

// TestVectorRawStringQuery 验证兼容图节点直传裸字符串(node 模式)。
func TestVectorRawStringQuery(t *testing.T) {
	src, _ := source.New(context.Background(), "vector", "kb", map[string]any{
		"docs": []any{"退货政策:7天无理由"},
	})
	caps, _ := src.Sync(context.Background())
	out, err := capability.Invoke(context.Background(), caps[0], "退货")
	if err != nil || !strings.Contains(out, "7天无理由") {
		t.Fatalf("raw-string query: %q %v", out, err)
	}
}
