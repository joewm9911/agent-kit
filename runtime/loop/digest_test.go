package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	kvstore "github.com/joewm9911/agent-kit/protocol/store"
)

func memResultStore() *ResultStore { return NewResultStore(kvstore.NewInMemory(), 0) }

func bigTool(name string, out string) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: name},
	}, func(ctx context.Context, _ string) (string, error) { return out, nil })
}

func TestDigestOverThreshold(t *testing.T) {
	raw := strings.Repeat("日志行\n", 2000) // 8000 字符
	m := testmodel.New(schema.AssistantMessage("要点:三次超时,错误码 504", nil))
	caps := DigestResults([]capability.Capability{bigTool("search", raw)}, m, 4000)

	store := memResultStore()
	ctx := WithResultStore(runctx.WithInput(context.Background(), "查支付超时原因"), store)
	out, err := capability.Invoke(ctx, caps[0], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "结果已消化") || !strings.Contains(out, "错误码 504") {
		t.Fatalf("got %q", out)
	}
	if !strings.Contains(out, "r1") {
		t.Fatalf("digest should carry retrieval pointer, got %q", out)
	}
	// 全文进了暂存
	if full, ok := store.Get(ctx, "r1"); !ok || len([]rune(full)) != 8000 {
		t.Fatalf("full result not stored: ok=%v len=%d", ok, len([]rune(full)))
	}
	// 消化器收到了当前任务背景
	if m.Calls != 1 {
		t.Fatalf("digest model calls = %d", m.Calls)
	}
}

func TestDigestUnderThresholdPassthrough(t *testing.T) {
	m := testmodel.New()
	caps := DigestResults([]capability.Capability{bigTool("small", "短结果")}, m, 4000)
	ctx := WithResultStore(context.Background(), memResultStore())
	out, _ := capability.Invoke(ctx, caps[0], `{}`)
	if out != "短结果" || m.Calls != 0 {
		t.Fatalf("small result should pass through untouched: %q calls=%d", out, m.Calls)
	}
}

func TestDigestNoStoreFallsBack(t *testing.T) {
	raw := strings.Repeat("x", 9000)
	m := testmodel.New()
	caps := DigestResults([]capability.Capability{bigTool("search", raw)}, m, 4000)
	// ctx 无暂存(库方式直接调用):原样返回,不消化
	out, _ := capability.Invoke(context.Background(), caps[0], `{}`)
	if len(out) != 9000 || m.Calls != 0 {
		t.Fatalf("no store should mean raw passthrough: len=%d calls=%d", len(out), m.Calls)
	}
}

func TestDigestRawTagExempt(t *testing.T) {
	raw := strings.Repeat("x", 9000)
	exempt := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Domain: "t", Name: "dump"},
		Tags: []string{TagRawResult},
	}, func(ctx context.Context, _ string) (string, error) { return raw, nil })
	m := testmodel.New()
	caps := DigestResults([]capability.Capability{exempt}, m, 4000)
	ctx := WithResultStore(context.Background(), memResultStore())
	out, _ := capability.Invoke(ctx, caps[0], `{}`)
	if len(out) != 9000 || m.Calls != 0 {
		t.Fatalf("TagRawResult should bypass digest: len=%d calls=%d", len(out), m.Calls)
	}
}

func TestReadResultPaging(t *testing.T) {
	store := memResultStore()
	full := strings.Repeat("甲", 5000)
	ctx := WithResultStore(context.Background(), store)
	id := store.Put(ctx, "search", full)
	rr := ReadResult()

	out, err := capability.Invoke(ctx, rr, `{"id":"`+id+`"}`)
	if err != nil || !strings.Contains(out, "[0-3000 / 共 5000 字符]") {
		t.Fatalf("got %q %v", out, err)
	}
	out, _ = capability.Invoke(ctx, rr, `{"id":"`+id+`","offset":3000}`)
	if !strings.Contains(out, "[3000-5000 / 共 5000 字符]") {
		t.Fatalf("got %q", out)
	}
	out, _ = capability.Invoke(ctx, rr, `{"id":"ghost"}`)
	if !strings.Contains(out, "不存在") {
		t.Fatalf("got %q", out)
	}
}

func TestForkMessages(t *testing.T) {
	task := schema.UserMessage("分析日志")
	snap := []*schema.Message{
		schema.UserMessage("payment 服务出问题了"),
		schema.AssistantMessage("已排除网络原因", nil),
	}

	// 未请求 fork:只有任务
	msgs := ForkMessages(WithConversationSnapshot(context.Background(), snap), task)
	if len(msgs) != 1 {
		t.Fatalf("without fork request: %d msgs", len(msgs))
	}
	// 请求 fork 且有快照:背景标注 + 快照 + 任务
	ctx := runctx.WithForkContext(WithConversationSnapshot(context.Background(), snap))
	msgs = ForkMessages(ctx, task)
	if len(msgs) != 4 {
		t.Fatalf("forked: %d msgs", len(msgs))
	}
	if msgs[0].Role != schema.System || !strings.Contains(msgs[1].Content, "payment") || msgs[3] != task {
		t.Fatalf("forked shape wrong: %+v", msgs)
	}
	// 请求 fork 但无快照:退化为只有任务
	msgs = ForkMessages(runctx.WithForkContext(context.Background()), task)
	if len(msgs) != 1 {
		t.Fatalf("fork without snapshot: %d msgs", len(msgs))
	}
}
