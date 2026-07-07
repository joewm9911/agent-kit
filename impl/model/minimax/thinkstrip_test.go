package minimax

import (
	"context"
	"io"
	"strings"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestStripLeadingThink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<think>推理过程 {e1} 示例</think>\n\n最终回答", "最终回答"},
		{"  \n<think>x</think>答案", "答案"},
		{"没有思考块的普通回答", "没有思考块的普通回答"},
		{"<think>未闭合的思考", "<think>未闭合的思考"}, // 保守不动
		{"正文里出现 <think> 不在开头</think>", "正文里出现 <think> 不在开头</think>"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripLeadingThink(c.in); got != c.want {
			t.Fatalf("stripLeadingThink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeStreamModel 按给定 chunk 序列回放流(每个字符串一帧)。
type fakeStreamModel struct{ chunks []string }

func (f *fakeStreamModel) Generate(context.Context, []*schema.Message, ...einomodel.Option) (*schema.Message, error) {
	return schema.AssistantMessage(strings.Join(f.chunks, ""), nil), nil
}

func (f *fakeStreamModel) Stream(context.Context, []*schema.Message, ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](len(f.chunks))
	for _, c := range f.chunks {
		sw.Send(schema.AssistantMessage(c, nil), nil)
	}
	sw.Close()
	return sr, nil
}

func (f *fakeStreamModel) WithTools([]*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return f, nil
}

func collectStream(t *testing.T, m einomodel.ToolCallingChatModel) string {
	t.Helper()
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	var sb strings.Builder
	for {
		f, err := sr.Recv()
		if err == io.EOF {
			return sb.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(f.Content)
	}
}

func TestThinkStripStream(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"标签整帧", []string{"<think>思考中 {示例}</think>", "\n\n答案A"}, "答案A"},
		{"开标签跨帧切分", []string{"<thi", "nk>abc</think>答案B"}, "答案B"},
		{"闭标签跨帧切分", []string{"<think>abc</thi", "nk>", "答案C"}, "答案C"},
		{"无思考块", []string{"直接", "回答"}, "直接回答"},
		{"疑似前缀但不是", []string{"<th", "e answer is 42"}, "<the answer is 42"},
		{"流结束仍是疑似前缀", []string{"<thi"}, "<thi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &thinkStripModel{inner: &fakeStreamModel{chunks: c.chunks}}
			if got := collectStream(t, m); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestThinkStripStreamToolCalls:think 吞噬期间,携带 tool_calls 的帧即刻
// 放行(content 置空),不被缓冲延迟。
func TestThinkStripStreamToolCalls(t *testing.T) {
	tc := schema.ToolCall{ID: "1", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}
	inner := &fakeStreamModel{}
	sr, sw := schema.Pipe[*schema.Message](3)
	sw.Send(schema.AssistantMessage("<think>推理", nil), nil)
	sw.Send(&schema.Message{Role: schema.Assistant, Content: "更多推理", ToolCalls: []schema.ToolCall{tc}}, nil)
	sw.Send(schema.AssistantMessage("</think>收尾", nil), nil)
	sw.Close()
	_ = inner

	out, w := schema.Pipe[*schema.Message](3)
	go func() {
		defer w.Close()
		st := &thinkStreamState{}
		for {
			m, err := sr.Recv()
			if err == io.EOF {
				return
			}
			for _, o := range st.feed(m) {
				w.Send(o, nil)
			}
		}
	}()
	var toolSeen bool
	var text strings.Builder
	for {
		f, err := out.Recv()
		if err == io.EOF {
			break
		}
		if len(f.ToolCalls) > 0 {
			toolSeen = true
			if f.Content != "" {
				t.Fatalf("tool 帧的思考文本未吞: %q", f.Content)
			}
		}
		text.WriteString(f.Content)
	}
	if !toolSeen {
		t.Fatal("tool_calls 帧被吞掉")
	}
	if text.String() != "收尾" {
		t.Fatalf("text = %q, want 收尾", text.String())
	}
}

// TestThinkStripGenerate:Generate 路径剥离且不改上游消息对象。
func TestThinkStripGenerate(t *testing.T) {
	m := &thinkStripModel{inner: &fakeStreamModel{chunks: []string{"<think>x</think>好的"}}}
	out, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil || out.Content != "好的" {
		t.Fatalf("generate: %v %+v", err, out)
	}
}
