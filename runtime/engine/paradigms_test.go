package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

func plainCap(name, desc string, fn func(ctx context.Context, args string) (string, error)) capability.Capability {
	return capability.New(capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "t", Name: name},
		Description: desc,
	}, fn)
}

// ---- reflection ----

func TestReflectionPassesAfterRevision(t *testing.T) {
	// 脚本:初稿 → 评审不通过 → 修订稿 → 评审通过
	m := testmodel.New(
		schema.AssistantMessage("草稿v1", nil),
		schema.AssistantMessage(`{"pass": false, "feedback": "缺少数据支撑"}`, nil),
		schema.AssistantMessage("草稿v2(带数据)", nil),
		schema.AssistantMessage(`{"pass": true}`, nil),
	)
	r, err := Build(context.Background(), "reflection", &Assembly{Model: m})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("写报告")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "草稿v2(带数据)" {
		t.Fatalf("got %q", out.Content)
	}
	if m.Calls != 4 {
		t.Fatalf("model calls = %d, want 4", m.Calls)
	}
}

func TestReflectionExhaustedBest(t *testing.T) {
	// 评审永不通过,max_rounds=2,best 策略返回最新稿
	m := testmodel.New(
		schema.AssistantMessage("v1", nil),
		schema.AssistantMessage(`{"pass": false, "feedback": "改"}`, nil),
		schema.AssistantMessage("v2", nil),
		schema.AssistantMessage(`{"pass": false, "feedback": "再改"}`, nil),
		schema.AssistantMessage("v3", nil),
	)
	r, err := Build(context.Background(), "reflection", &Assembly{
		Model: m, Config: map[string]any{"max_rounds": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("写")})
	if err != nil || out.Content != "v3" {
		t.Fatalf("best should return latest draft, got %q %v", out.Content, err)
	}
}

func TestReflectionExhaustedError(t *testing.T) {
	m := testmodel.New(
		schema.AssistantMessage("v1", nil),
		schema.AssistantMessage(`{"pass": false, "feedback": "改"}`, nil),
		schema.AssistantMessage("v2", nil),
	)
	r, err := Build(context.Background(), "reflection", &Assembly{
		Model: m, Config: map[string]any{"max_rounds": 1, "on_exhausted": "error"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("写")}); err == nil {
		t.Fatal("expect exhausted error")
	}
}

// ---- P2.5 多阶段全透:阶段提示词渲染 params + 内置变量 ----

// captureModel 记录最近一次收到的消息,用于断言阶段提示词已被渲染。
type captureModel struct {
	reply  *schema.Message
	lastIn []*schema.Message
}

func (c *captureModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	c.lastIn = in
	return c.reply, nil
}
func (c *captureModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	c.lastIn = in
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(c.reply, nil)
	sw.Close()
	return sr, nil
}
func (c *captureModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return c, nil
}

// TestStagePromptRendersVars:router 的 route 阶段提示词经 renderStage 获得
// params({topic})与内置 {$user_input};ctx 无 vars 袋时占位符原样保留。
func TestStagePromptRendersVars(t *testing.T) {
	cap0 := plainCap("x", "任意", func(_ context.Context, _ string) (string, error) { return "ok", nil })
	cm := &captureModel{reply: schema.AssistantMessage(`{"target":"x","args":{}}`, nil)}
	r, err := Build(context.Background(), "router", &Assembly{
		Model:        cm,
		Capabilities: []capability.Capability{cap0},
		Prompts:      map[string]string{"route": "分诊器。主题={topic};原问题={$user_input}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runctx.WithVars(context.Background(), map[string]string{"topic": "音频", "$user_input": "原始问题"})
	if _, err := r.Generate(ctx, []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatal(err)
	}
	sys := cm.lastIn[0].Content
	if !strings.Contains(sys, "主题=音频") || !strings.Contains(sys, "原问题=原始问题") {
		t.Fatalf("stage prompt not rendered with vars: %q", sys)
	}

	// 无 vars 袋:占位符原样保留(向后兼容)
	cm.lastIn = nil
	if _, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatal(err)
	}
	if sys := cm.lastIn[0].Content; !strings.Contains(sys, "{topic}") {
		t.Fatalf("no-vars path should keep placeholder literal: %q", sys)
	}
}

// ---- router ----

func TestRouterDispatch(t *testing.T) {
	var gotArgs atomic.Value
	metrics := plainCap("metrics_qa", "回答指标数据类问题", func(_ context.Context, args string) (string, error) {
		gotArgs.Store(args)
		return "指标答案", nil
	})
	chat := plainCap("small_talk", "闲聊", func(_ context.Context, args string) (string, error) {
		return "闲聊答案", nil
	})

	m := testmodel.New(schema.AssistantMessage(`{"target":"metrics_qa","args":{"q":"上季度GMV"}}`, nil))
	r, err := Build(context.Background(), "router", &Assembly{
		Model: m, Capabilities: []capability.Capability{metrics, chat},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("上季度GMV多少")})
	if err != nil || out.Content != "指标答案" {
		t.Fatalf("got %q %v", out.Content, err)
	}
	if !strings.Contains(gotArgs.Load().(string), "上季度GMV") {
		t.Fatalf("target args = %v", gotArgs.Load())
	}
	if m.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (纯分诊)", m.Calls)
	}
}

func TestRouterFallback(t *testing.T) {
	var fallbackHit int32
	chat := plainCap("small_talk", "闲聊", func(_ context.Context, args string) (string, error) {
		atomic.AddInt32(&fallbackHit, 1)
		return "兜底答案", nil
	})
	// 模型输出无法解析 → 走 fallback
	m := testmodel.New(schema.AssistantMessage("我觉得应该……", nil))
	r, err := Build(context.Background(), "router", &Assembly{
		Model: m, Capabilities: []capability.Capability{chat},
		Config: map[string]any{"fallback": "small_talk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("嗯?")})
	if err != nil || out.Content != "兜底答案" || fallbackHit != 1 {
		t.Fatalf("got %q %v hit=%d", out.Content, err, fallbackHit)
	}

	// 无 fallback → 报错
	m2 := testmodel.New(schema.AssistantMessage("???", nil))
	r2, _ := Build(context.Background(), "router", &Assembly{
		Model: m2, Capabilities: []capability.Capability{chat},
	})
	if _, err := r2.Generate(context.Background(), []*schema.Message{schema.UserMessage("嗯?")}); err == nil {
		t.Fatal("expect route resolution error without fallback")
	}
}

// ---- rewoo ----

func TestRewooPlanExecuteSolve(t *testing.T) {
	var calls []string
	search := plainCap("search", "检索", func(_ context.Context, args string) (string, error) {
		calls = append(calls, "search:"+args)
		return "结果A", nil
	})
	enrich := plainCap("enrich", "补充", func(_ context.Context, args string) (string, error) {
		calls = append(calls, "enrich:"+args)
		return "结果B", nil
	})

	// 脚本:①规划(e2 引用 {e1})②求解
	m := testmodel.New(
		schema.AssistantMessage(`{"steps":[
			{"id":"e1","tool":"search","args":{"q":"营收"}},
			{"id":"e2","tool":"enrich","args":{"base":"{e1}","extra":"环比"}}
		]}`, nil),
		schema.AssistantMessage("最终分析", nil),
	)
	r, err := Build(context.Background(), "rewoo", &Assembly{
		Model: m, Capabilities: []capability.Capability{search, enrich},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("分析营收")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "最终分析" {
		t.Fatalf("got %q", out.Content)
	}
	if m.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (规划+求解)", m.Calls)
	}
	// e2 的 {e1} 被替换成了 e1 的结果
	if len(calls) != 2 || !strings.Contains(calls[1], `"base":"结果A"`) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestRewooRejectsForwardRef(t *testing.T) {
	echo := plainCap("echo", "回显", func(_ context.Context, args string) (string, error) { return args, nil })
	m := testmodel.New(schema.AssistantMessage(`{"steps":[
		{"id":"e1","tool":"echo","args":{"q":"{e2}"}},
		{"id":"e2","tool":"echo","args":{"q":"x"}}
	]}`, nil))
	r, err := Build(context.Background(), "rewoo", &Assembly{
		Model: m, Capabilities: []capability.Capability{echo},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err == nil ||
		!strings.Contains(err.Error(), "earlier step") {
		t.Fatalf("expect forward-ref rejection, got %v", err)
	}
}

func TestRewooUnknownToolAsEvidence(t *testing.T) {
	echo := plainCap("echo", "回显", func(_ context.Context, args string) (string, error) { return args, nil })
	m := testmodel.New(
		schema.AssistantMessage(`{"steps":[{"id":"e1","tool":"ghost","args":{}}]}`, nil),
		schema.AssistantMessage("如实说明失败", nil),
	)
	r, err := Build(context.Background(), "rewoo", &Assembly{
		Model: m, Capabilities: []capability.Capability{echo},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "如实说明失败" {
		t.Fatalf("unknown tool should flow to solver as evidence: %q %v", out.Content, err)
	}
}
