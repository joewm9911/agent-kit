// thinkstrip.go:推理模型 <think> 块的适配层剥离。
//
// MiniMax M 系经 OpenAI 兼容接口把推理过程以 "<think>...</think>" 内联
// 在 content 开头。思考文本不该回填上下文(各推理模型厂商的通用实践):
// 回填徒增后续轮次 token、污染压缩摘要,终端用户还会直接看到思考过程。
// 在适配层剥除一次,主循环/引擎/技能全部受益;内层模型的回调看到的仍是
// 原文,观测轨迹不丢推理过程。
//
// 剥离语义:只剥**开头**的一个 think 块(M 系的实际形态);未闭合的
// think(截断/异常输出)保守不动,交给上层守卫处理。
package minimax

import (
	"context"
	"io"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkStripModel 包装内层模型,Generate/Stream 返回前剥除开头 think 块。
type thinkStripModel struct {
	inner einomodel.ToolCallingChatModel
}

func (t *thinkStripModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	inner, err := t.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &thinkStripModel{inner: inner}, nil
}

func (t *thinkStripModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	out, err := t.inner.Generate(ctx, msgs, opts...)
	if err != nil || out == nil {
		return out, err
	}
	c := *out
	c.Content = stripLeadingThink(out.Content)
	return &c, nil
}

func (t *thinkStripModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, err := t.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	out, w := schema.Pipe[*schema.Message](8)
	go func() {
		defer sr.Close()
		defer w.Close()
		st := &thinkStreamState{}
		for {
			m, err := sr.Recv()
			if err == io.EOF {
				if tail := st.flush(); tail != nil && w.Send(tail, nil) {
					return
				}
				return
			}
			if err != nil {
				w.Send(nil, err)
				return
			}
			for _, o := range st.feed(m) {
				if w.Send(o, nil) {
					return
				}
			}
		}
	}()
	return out, nil
}

// stripLeadingThink 剥除开头的一个完整 think 块(允许前导空白)。
func stripLeadingThink(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(t, thinkOpen) {
		return s
	}
	i := strings.Index(t, thinkClose)
	if i < 0 {
		return s // 未闭合:保守不动
	}
	return strings.TrimLeft(t[i+len(thinkClose):], " \t\r\n")
}

// thinkStreamState 是流式剥离状态机。三态:
//
//	detect  开头判定期:缓冲文本直到能确定是否以 <think> 开头
//	        (标签可能被 chunk 切开,如 "<thi" + "nk>");
//	drop    think 块内:丢弃文本直到 </think>(同样容忍跨 chunk 切分);
//	trim    剥离刚完成:下一个非空文本帧去掉前导空白后转 pass
//	        (think 块与正文之间的空行常落在下一帧);
//	pass    透传期:think 已剥完或确认不存在。
//
// 任何携带非文本载荷的帧(tool_calls 等)即刻放行(文本载荷按状态处理),
// 不因缓冲延迟工具调用。
type thinkStreamState struct {
	mode int // 0=detect 1=drop 2=pass 3=trim
	buf  strings.Builder
	m    *schema.Message // detect 期最近一帧的形状(flush 时还原用)
}

func (st *thinkStreamState) feed(m *schema.Message) []*schema.Message {
	if st.mode == 2 || m == nil {
		return []*schema.Message{m}
	}
	if m.Content == "" {
		return []*schema.Message{m} // 纯载荷帧(tool_calls/usage)直接放行
	}
	if st.mode == 3 { // trim:去前导空白后转 pass
		trimmed := strings.TrimLeft(m.Content, " \t\r\n")
		if trimmed == "" {
			return st.holdover(m) // 整帧都是空白,继续等正文
		}
		st.mode = 2
		return []*schema.Message{withContent(m, trimmed)}
	}
	st.m = m
	switch st.mode {
	case 0: // detect
		st.buf.WriteString(m.Content)
		trimmed := strings.TrimLeft(st.buf.String(), " \t\r\n")
		switch {
		case len(trimmed) < len(thinkOpen) && strings.HasPrefix(thinkOpen, trimmed):
			return st.holdover(m) // 仍可能是 think 开头,继续缓冲
		case strings.HasPrefix(trimmed, thinkOpen):
			st.mode = 1
			st.buf.Reset()
			// 把 <think> 之后的部分交给 drop 态继续判定
			return st.dropFeed(m, trimmed[len(thinkOpen):])
		default: // 确认不是 think 开头:一次性放出全部缓冲
			held := st.buf.String()
			st.buf.Reset()
			st.mode = 2
			return []*schema.Message{withContent(m, held)}
		}
	default: // drop
		return st.dropFeed(m, m.Content)
	}
}

// dropFeed 处理 think 块内的文本:找到闭合标签则放出其后的内容。
func (st *thinkStreamState) dropFeed(m *schema.Message, chunk string) []*schema.Message {
	st.buf.WriteString(chunk)
	s := st.buf.String()
	if i := strings.Index(s, thinkClose); i >= 0 {
		st.buf.Reset()
		after := strings.TrimLeft(s[i+len(thinkClose):], " \t\r\n")
		if after == "" {
			st.mode = 3 // 正文在后续帧,首帧去前导空白
			return st.holdover(m)
		}
		st.mode = 2
		return []*schema.Message{withContent(m, after)}
	}
	// 闭合标签可能被切开:只保留足以拼出 </think> 的尾巴,内存有界
	if tail := len(thinkClose) - 1; len(s) > tail {
		st.buf.Reset()
		st.buf.WriteString(s[len(s)-tail:])
	}
	return st.holdover(m)
}

// holdover 在吞掉文本的同时放行帧上的非文本载荷(若有)。
func (st *thinkStreamState) holdover(m *schema.Message) []*schema.Message {
	if len(m.ToolCalls) > 0 {
		return []*schema.Message{withContent(m, "")}
	}
	return nil
}

// flush 在流结束时兜底:detect 期缓冲的疑似前缀原样放出
// (drop 期的未闭合 think 已无法还原完整原文,不再吐回)。
func (st *thinkStreamState) flush() *schema.Message {
	if st.mode == 0 && st.buf.Len() > 0 && st.m != nil {
		out := withContent(st.m, st.buf.String())
		// 帧上的非文本载荷(tool_calls)在 holdover 时已放行过,这里
		// 再带一遍会让下游收到重复的 ToolCalls。
		out.ToolCalls = nil
		return out
	}
	return nil
}

// withContent 返回替换了 Content 的浅拷贝(不改上游帧)。
func withContent(m *schema.Message, content string) *schema.Message {
	c := *m
	c.Content = content
	return &c
}
