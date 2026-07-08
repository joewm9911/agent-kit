package serving

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

func postMessage(t *testing.T, s *Server, body map[string]string) (int, map[string]string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/agents/a/messages", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestHTTPSuspendResume:/messages 的挂起-恢复与 dispatcher 的 IM 路径
// 同一机制:首轮在 ask_user 挂起,响应 waiting + 问句(HTTP 的"送达"
// 就是响应体);同会话下一个请求是答案,重放该轮后响应最终答案。
func TestHTTPSuspendResume(t *testing.T) {
	ag := agent.New("a", "", askRunner{}, nil, agent.Options{})
	s := New(":0", []Runnable{ag}, nil)
	s.EnableSuspend(store.NewInMemory())

	code, r1 := postMessage(t, s, map[string]string{"session": "s1", "input": "帮我查天气"})
	if code != 200 || r1["status"] != "waiting" || !strings.Contains(r1["question"], "哪个城市") {
		t.Fatalf("first turn should suspend with question: code=%d resp=%v", code, r1)
	}
	code, r2 := postMessage(t, s, map[string]string{"session": "s1", "input": "北京"})
	if code != 200 || r2["status"] != "done" || !strings.Contains(r2["answer"], "已确认城市:北京") {
		t.Fatalf("resume should replay with answer: code=%d resp=%v", code, r2)
	}
	// 挂起已消费:同会话再来一条是全新一轮,不应吃到旧答案
	if _, r3 := postMessage(t, s, map[string]string{"session": "s1", "input": "再查一次"}); r3["status"] != "waiting" {
		t.Fatalf("new turn should suspend afresh: %v", r3)
	}
}

// TestHTTPSuspendCrossInstance:挂起与恢复落在不同 Server 实例(共享
// 持久 KV)——进程重启 / 多副本部署下答案照样接得上。
func TestHTTPSuspendCrossInstance(t *testing.T) {
	kv := store.NewInMemory()
	ag := agent.New("a", "", askRunner{}, nil, agent.Options{})

	s1 := New(":0", []Runnable{ag}, nil)
	s1.EnableSuspend(kv)
	if _, r := postMessage(t, s1, map[string]string{"session": "s9", "input": "帮我查天气"}); r["status"] != "waiting" {
		t.Fatalf("suspend on instance 1: %v", r)
	}

	s2 := New(":0", []Runnable{ag}, nil)
	s2.EnableSuspend(kv)
	if _, r := postMessage(t, s2, map[string]string{"session": "s9", "input": "上海"}); r["status"] != "done" ||
		!strings.Contains(r["answer"], "已确认城市:上海") {
		t.Fatalf("resume on instance 2: %v", r)
	}
}

// effectRunner:提问前先过一次效果日志闸门(与 DurableEffects 的
// runDurable 同语义),断言重放不二次执行。命中的前提是重放轮的
// journal 在 ctx 且 turnID 与首跑一致——这正是 HTTP 接线要保证的。
type effectRunner struct {
	sideEffects *atomic.Int32
}

func (r effectRunner) Generate(ctx context.Context, _ []*schema.Message) (*schema.Message, error) {
	j := suspend.JournalFrom(ctx)
	if j == nil {
		return nil, context.Canceled // 接线断了:挂起模式下 journal 必须随 ctx 下发
	}
	if _, ok := j.Effect(ctx, "tool/live/deduct", `{"order":"o1"}`); !ok {
		r.sideEffects.Add(1)
		j.SaveEffect(ctx, "tool/live/deduct", `{"order":"o1"}`, "扣减完成")
	}
	ans, err := runctx.GetInteractor(ctx).Ask(ctx, "扣减已执行,继续吗?")
	if err != nil {
		return nil, err
	}
	return schema.AssistantMessage("收尾:"+ans, nil), nil
}

func (r effectRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, err := r.Generate(ctx, msgs)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestHTTPSuspendEffectIdempotent:重放路径上 mutating 效果命中效果
// 日志,不二次执行(挂起前副作用只发生一次)。
func TestHTTPSuspendEffectIdempotent(t *testing.T) {
	var n atomic.Int32
	ag := agent.New("a", "", effectRunner{sideEffects: &n}, nil, agent.Options{})
	s := New(":0", []Runnable{ag}, nil)
	s.EnableSuspend(store.NewInMemory())

	if _, r := postMessage(t, s, map[string]string{"session": "s2", "input": "扣库存"}); r["status"] != "waiting" {
		t.Fatalf("should suspend after effect: %v", r)
	}
	if _, r := postMessage(t, s, map[string]string{"session": "s2", "input": "继续"}); r["status"] != "done" {
		t.Fatalf("resume: %v", r)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("mutating effect should run exactly once, ran %d times", got)
	}
}

// plainRunner 不需要交互,直接作答。
type plainRunner struct{}

func (plainRunner) Generate(context.Context, []*schema.Message) (*schema.Message, error) {
	return schema.AssistantMessage("好的", nil), nil
}

func (r plainRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	out, _ := r.Generate(ctx, msgs)
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// TestHTTPMessagePlain:未启用 suspend 的直答路径,响应带 status: done
// (与挂起路径的响应形态一致,调用方按 status 分流)。
func TestHTTPMessagePlain(t *testing.T) {
	ag := agent.New("a", "", plainRunner{}, nil, agent.Options{})
	s := New(":0", []Runnable{ag}, nil)

	code, r := postMessage(t, s, map[string]string{"session": "s3", "input": "你好"})
	if code != 200 || r["status"] != "done" || r["answer"] != "好的" {
		t.Fatalf("plain path: code=%d resp=%v", code, r)
	}
}
