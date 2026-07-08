package config

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/memory"
	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// countingRetriever 记录调用次数并返回固定片段。
type countingRetriever struct{ hits atomic.Int32 }

func (r *countingRetriever) Retrieve(_ context.Context, _ []*schema.Message, _ string, _ int) []string {
	r.hits.Add(1)
	return []string{"user: 早前提过预算是100万"}
}

var registerCountingRetriever sync.Once

func setupCountingRetriever() {
	registerCountingRetriever.Do(func() {
		session.RegisterRetriever("counting", func(_ map[string]any) (session.Retriever, error) {
			return &countingRetriever{}, nil
		})
	})
}

// recallCtx 构造带窗外历史与用户身份的调用环境(窗口 2,历史 4 条 →
// 2 条在窗外)。用户身份让长期召回能命中用户桶。
func recallCtx(sessionID, input string) context.Context {
	ctx := runctx.With(context.Background(), "a", sessionID)
	ctx = runctx.WithUser(ctx, "u1")
	ctx = runctx.WithInput(ctx, input)
	return loop.WithTurnHistory(ctx, []*schema.Message{
		schema.UserMessage("早前提过预算是100万"),
		schema.AssistantMessage("记下了", nil),
		schema.UserMessage("最近的话"),
		schema.AssistantMessage("好的", nil),
	})
}

// TestAutoRecallSplitPaths 验证两路召回独立开关:关掉的那一路
// 完全不被触碰(不是查了不用,而是根本不查)。
func TestAutoRecallSplitPaths(t *testing.T) {
	kv, _ := memory.New("inmemory", nil)
	_ = kv.Put(context.Background(), memory.UserScope("u1"), "预算", "100万")
	retr := &countingRetriever{}
	scope := memory.ScopeConfig{} // 缺省:写 user、读 user+shared

	// 双路开启:两种来源都出现
	both := autoRecall(kv, scope, retr, 2, 2, 3)
	joined := strings.Join(both(recallCtx("s1", "预算")), "\n")
	if !strings.Contains(joined, "Long-term memory") || !strings.Contains(joined, "Earlier conversation") {
		t.Fatalf("both paths expected: %q", joined)
	}

	// 只开 session 路:KV 不出现
	sessOnly := autoRecall(kv, scope, retr, 2, 2, 0)
	joined = strings.Join(sessOnly(recallCtx("s2", "预算")), "\n")
	if strings.Contains(joined, "Long-term memory") || !strings.Contains(joined, "Earlier conversation") {
		t.Fatalf("session-only expected: %q", joined)
	}

	// 只开 long_term 路:检索器完全不被调用
	before := retr.hits.Load()
	kvOnly := autoRecall(kv, scope, retr, 2, 0, 3)
	joined = strings.Join(kvOnly(recallCtx("s3", "预算")), "\n")
	if !strings.Contains(joined, "Long-term memory") || strings.Contains(joined, "Earlier conversation") {
		t.Fatalf("kv-only expected: %q", joined)
	}
	if retr.hits.Load() != before {
		t.Fatal("retriever must not be consulted when session path is off")
	}
}

// TestRetrieverRegistry 验证注册表:缺省 bigram、按名解析、未知名报错。
func TestRetrieverRegistry(t *testing.T) {
	setupCountingRetriever()

	// 空名 → 缺省 bigram,行为与 SearchRelevant 一致
	r, err := session.NewRetriever("", nil)
	if err != nil {
		t.Fatal(err)
	}
	history := []*schema.Message{schema.UserMessage("数据库迁移方案已经确认")}
	hits := r.Retrieve(context.Background(), history, "数据库迁移", 3)
	if len(hits) != 1 || !strings.Contains(hits[0], "数据库迁移") {
		t.Fatalf("bigram default: %v", hits)
	}

	// 注册名解析
	if _, err := session.NewRetriever("counting", nil); err != nil {
		t.Fatal(err)
	}
	// 未知名:明确报错并列出已注册项
	if _, err := session.NewRetriever("ghost", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown retriever") {
		t.Fatalf("expect unknown retriever error, got %v", err)
	}
}

// TestRecallConfigResolution 验证装配语义:session/memory 两路各自独立
// 配置、负值显式关闭、未注册检索器装配期拒绝。
func TestRecallConfigResolution(t *testing.T) {
	setupAppTestFakes()
	setupCountingRetriever()

	build := func(agentYAML string) (*App, error) {
		t.Helper()
		appPath := writeTree(t, map[string]string{
			"app.yaml": `
model: {provider: marker, config: {resp: m}}
agents: [agents/a.yaml]
`,
			"agents/a.yaml": agentYAML,
		})
		spec, err := LoadApp(appPath)
		if err != nil {
			t.Fatal(err)
		}
		return BuildApp(context.Background(), spec, BuildOptions{})
	}

	// 两路独立开、指定检索器、负值关闭:均合法装配
	for _, yaml := range []string{
		"session: {window: 10, recall: {top_k: 2}}\nmemory: {recall: {top_k: 3}}\n",
		"session:\n  window: 10\n  recall: {top_k: 2, retriever: counting}\nmemory:\n  recall: {top_k: 3}\n",
		"session: {window: 10, recall: {top_k: -1}}\n",
	} {
		if _, err := build(yaml); err != nil {
			t.Fatalf("valid config rejected: %v\n%s", err, yaml)
		}
	}

	// 未注册的检索器名:装配期拒绝(fail fast)
	if _, err := build("session: {window: 10, recall: {top_k: 2, retriever: ghost}}\n"); err == nil ||
		!strings.Contains(err.Error(), "unknown retriever") {
		t.Fatalf("expect assembly-time rejection, got %v", err)
	}
}
