package serving

// 交付物出站解析 + dispatcher 随行链路测试。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestDispatcherDeliverableFollowup:IM 路径终答之后,引用的交付物作为
// KindDeliverable 独立消息随行,原文零损耗。
func TestDispatcherDeliverableFollowup(t *testing.T) {
	fc := &fakeChannel{}
	ag := agent.New("a", "", deliverRunner{}, nil, agent.Options{})
	d := NewDispatcher(nil)
	h := d.Handler(Binding{Channel: fc, Agent: ag})

	conv := channel.ConvRef{Channel: "fake", Chat: "c-del", User: "u1"}
	h(context.Background(), channel.Inbound{Conv: conv, Text: "出月报", EventID: "ed1"})

	waitFor(t, func() bool { return len(fc.messages()) >= 2 })
	msgs := fc.messages()
	if !strings.Contains(msgs[0], "#d1") {
		t.Fatalf("answer should reference #d1, got %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "全量数据") || !strings.Contains(msgs[1], "#d1 · 月报") {
		t.Fatalf("follow-up must carry verbatim deliverable with default header, got %q", msgs[1])
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
