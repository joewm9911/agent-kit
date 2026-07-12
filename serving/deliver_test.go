package serving

// 交付物出站解析 + dispatcher 随行链路测试。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
)

func sinkWith(items ...runctx.Deliverable) *runctx.DeliverableSink {
	_, s := runctx.WithDeliverableSink(context.Background())
	for _, it := range items {
		s.Emit(it)
	}
	return s
}

func TestResolveDeliverables(t *testing.T) {
	sink := sinkWith(
		runctx.Deliverable{ID: "d1", Title: "报表", Mode: capability.DeliverAttach, Content: "A"},
		runctx.Deliverable{ID: "d2", Title: "盘点", Mode: capability.DeliverAttach, Content: "B"},
		runctx.Deliverable{ID: "d3", Title: "审计", Mode: capability.DeliverAlways, Content: "C"},
	)

	// 引用序优先,always 追加,幻觉忽略,同 id 去重
	out := ResolveDeliverables("先看 #d2,再看 #d1;#d2 重复引用;#d9 是幻觉", sink)
	ids := make([]string, 0, len(out))
	for _, d := range out {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "d2,d1,d3" {
		t.Fatalf("want d2,d1,d3 got %v", ids)
	}

	// 零引用:只有 always 随行
	out = ResolveDeliverables("只给结论,不引用任何交付物", sink)
	if len(out) != 1 || out[0].ID != "d3" {
		t.Fatalf("want only always item, got %v", out)
	}
}

func TestResolveDeliverablesGuards(t *testing.T) {
	// 数量护栏:7 个全引用只留 5
	items := make([]runctx.Deliverable, 0, 7)
	refs := make([]string, 0, 7)
	for i := 1; i <= 7; i++ {
		id := "d" + string(rune('0'+i))
		items = append(items, runctx.Deliverable{ID: id, Mode: capability.DeliverAttach, Content: "x"})
		refs = append(refs, "#"+id)
	}
	out := ResolveDeliverables(strings.Join(refs, " "), sinkWith(items...))
	if len(out) != maxDeliverables {
		t.Fatalf("count guard: want %d got %d", maxDeliverables, len(out))
	}
	// 体量护栏:两个 150KB 只留第一个
	big := strings.Repeat("x", 150<<10)
	out = ResolveDeliverables("#d1 #d2", sinkWith(
		runctx.Deliverable{ID: "d1", Mode: capability.DeliverAttach, Content: big},
		runctx.Deliverable{ID: "d2", Mode: capability.DeliverAttach, Content: big},
	))
	if len(out) != 1 || out[0].ID != "d1" {
		t.Fatalf("size guard: want [d1] got %d items", len(out))
	}
}

// deliverRunner 模拟"调 attach skill 后引用 #dN 作答"的一轮。
type deliverRunner struct{}

func (deliverRunner) Generate(ctx context.Context, _ []*schema.Message) (*schema.Message, error) {
	sink := runctx.DeliverableSinkFrom(ctx)
	sink.NextCallSeq()
	sink.Emit(runctx.Deliverable{ID: "d1", Title: "月报", Source: "cap://skill/t/report",
		Mode: capability.DeliverAttach, Content: "# 月报\n全量数据…"})
	// 实质导读(剥引用后仍有信息量,不触发裸引用折叠)
	return schema.AssistantMessage("本月总销量 2,724 件、销售额 ¥726,906,头部单品贡献超八成,建议加仓补货。完整报表见 #d1。", nil), nil
}

func (r deliverRunner) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, in)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestDispatcherDeliverableMerged:单份小交付物合并进终答卡,单条送达
// (导读在上、原文紧随),事实位仍带清单。
func TestDispatcherDeliverableMerged(t *testing.T) {
	fc := &fakeChannel{}
	ag := agent.New("a", "", deliverRunner{}, nil, agent.Options{})
	d := NewDispatcher(nil)
	h := d.Handler(Binding{Channel: fc, Agent: ag})

	conv := channel.ConvRef{Channel: "fake", Chat: "c-del", User: "u1"}
	h(context.Background(), channel.Inbound{Conv: conv, Text: "出月报", EventID: "ed1"})

	waitFor(t, func() bool { return len(fc.messages()) >= 1 })
	msgs := fc.messages()
	if !strings.Contains(msgs[0], "#d1") || !strings.Contains(msgs[0], "全量数据") {
		t.Fatalf("merged answer must carry lead + verbatim content, got %q", msgs[0])
	}
	// 稳定窗口内确认没有第二条(合并即单条)
	time.Sleep(300 * time.Millisecond)
	if n := len(fc.messages()); n != 1 {
		t.Fatalf("small single deliverable must be one message, got %d", n)
	}
}

// bigDeliverRunner 产出超过合并门的交付物(随行路径覆盖)。
type bigDeliverRunner struct{}

func (bigDeliverRunner) Generate(ctx context.Context, _ []*schema.Message) (*schema.Message, error) {
	sink := runctx.DeliverableSinkFrom(ctx)
	sink.NextCallSeq()
	sink.Emit(runctx.Deliverable{ID: "d1", Title: "大报表", Source: "cap://skill/t/report",
		Mode: capability.DeliverAttach, Content: "# 大报表\n" + strings.Repeat("行|", deliverMergeMax/2)})
	return schema.AssistantMessage("总量与结构详见完整大报表 #d1,头部集中度高,建议关注补货节奏。", nil), nil
}

func (r bigDeliverRunner) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, in)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestDispatcherDeliverableFollowup:超合并门的交付物维持"导读卡 + 随行卡",
// 原文零损耗在第二条。
func TestDispatcherDeliverableFollowup(t *testing.T) {
	fc := &fakeChannel{}
	ag := agent.New("a", "", bigDeliverRunner{}, nil, agent.Options{})
	d := NewDispatcher(nil)
	h := d.Handler(Binding{Channel: fc, Agent: ag})

	conv := channel.ConvRef{Channel: "fake", Chat: "c-del-big", User: "u1"}
	h(context.Background(), channel.Inbound{Conv: conv, Text: "出大报表", EventID: "ed2"})

	waitFor(t, func() bool { return len(fc.messages()) >= 2 })
	msgs := fc.messages()
	if strings.Contains(msgs[0], "行|行|") {
		t.Fatalf("oversized content must not merge into answer, got %d bytes", len(msgs[0]))
	}
	if !strings.Contains(msgs[1], "#d1 · 大报表") || !strings.Contains(msgs[1], "行|行|") {
		t.Fatalf("follow-up must carry verbatim big deliverable, got head %q", msgs[1][:80])
	}
}

// TestHTTPDeliverables:HTTP /messages 响应携带 deliverables 数组。
func TestHTTPDeliverables(t *testing.T) {
	ag := agent.New("a", "", deliverRunner{}, nil, agent.Options{})
	s := New(":0", []Runnable{ag}, nil)
	code, raw := postMessageRaw(t, s, map[string]string{"session": "s1", "input": "出月报"})
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(raw, `"deliverables"`) || !strings.Contains(raw, "全量数据") {
		t.Fatalf("response must carry deliverables verbatim, got %s", raw)
	}
}

// postMessageRaw 与 postMessage 同源,返回原始响应体(交付物数组是嵌套
// 结构,map[string]string 装不下)。
func postMessageRaw(t *testing.T, s *Server, body map[string]string) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/agents/a/messages", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestCollapseBareReference:终答只有裸引用时折叠为单条(实测:MiniMax
// 简洁性偏置常产出 "报表见 #d1。" 甚至 "#d1")。
func TestCollapseBareReference(t *testing.T) {
	dels := []runctx.Deliverable{
		{ID: "d1", Title: "报表", Content: "# 报表全文"},
		{ID: "d2", Title: "盘点", Content: "盘点全文"},
	}
	// 裸引用(纯 #d1)→ 折叠:首个交付物顶替终答,不再随行
	ans, rest := collapseBareReference("#d1", dels)
	if ans != "# 报表全文" || len(rest) != 1 || rest[0].ID != "d2" {
		t.Fatalf("bare ref must collapse, got ans=%q rest=%d", ans, len(rest))
	}
	// "报表见 #d1。" 这类空壳导读同样折叠
	ans, rest = collapseBareReference("报表见 #d1。", dels)
	if ans != "# 报表全文" || len(rest) != 1 {
		t.Fatalf("hollow lead must collapse, got %q", ans)
	}
	// 实质导读:不折叠
	lead := "总销量 2,724 件,头部商品贡献 81%,建议加仓补货。完整报表见 #d1。"
	ans, rest = collapseBareReference(lead, dels)
	if ans != lead || len(rest) != 2 {
		t.Fatalf("substantive lead must keep both, got %q rest=%d", ans, len(rest))
	}
	// 无交付物:原样
	ans, rest = collapseBareReference("#d1", nil)
	if ans != "#d1" || rest != nil {
		t.Fatal("no dels must be identity")
	}
}

// TestMergeSingleDeliverable:单份小交付物合并进终答卡;大/多份维持两条。
func TestMergeSingleDeliverable(t *testing.T) {
	small := runctx.Deliverable{ID: "d1", Title: "报表", Content: "# 报表全文"}
	// 小:合并,随行清空
	ans, rest := mergeSingleDeliverable("导读结论。见 #d1。", []runctx.Deliverable{small})
	if !strings.Contains(ans, "# 报表全文") || len(rest) != 0 {
		t.Fatalf("small single must merge, got ans=%q rest=%d", ans, len(rest))
	}
	// 大:不合并
	big := runctx.Deliverable{ID: "d1", Content: strings.Repeat("x", deliverMergeMax)}
	ans, rest = mergeSingleDeliverable("导读", []runctx.Deliverable{big})
	if strings.Contains(ans, "xxx") || len(rest) != 1 {
		t.Fatal("oversized must keep two messages")
	}
	// 多份:不合并
	ans, rest = mergeSingleDeliverable("导读", []runctx.Deliverable{small, {ID: "d2", Content: "y"}})
	if len(rest) != 2 || strings.Contains(ans, "报表全文") {
		t.Fatal("multiple must keep followups")
	}
}
