package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/runctx"
)

// CompactionConfig 控制上下文压缩,两个阈值任一超过即触发。
type CompactionConfig struct {
	// MaxMessages 触发压缩的消息条数阈值。
	MaxMessages int `yaml:"max_messages" json:"max_messages"`
	// MaxTokens 触发压缩的估算 token 阈值(缺平台用量时按 3 字符/token 估),
	// 用于按模型窗口而非消息条数控制。
	MaxTokens int `yaml:"max_tokens" json:"max_tokens"`
	// KeepRecent 压缩后保留的最近消息条数,默认 MaxMessages/2(或 10)。
	KeepRecent int `yaml:"keep_recent" json:"keep_recent"`
}

// Enabled 报告压缩是否启用。
func (c CompactionConfig) Enabled() bool { return c.MaxMessages > 0 || c.MaxTokens > 0 }

func (c CompactionConfig) Keep() int {
	if c.KeepRecent > 0 {
		return c.KeepRecent
	}
	if c.MaxMessages > 0 {
		return c.MaxMessages / 2
	}
	return 10
}

// Over 报告消息集是否超过任一阈值。
func (c CompactionConfig) Over(msgs []*schema.Message) bool {
	if c.MaxMessages > 0 && len(msgs) > c.MaxMessages {
		return true
	}
	if c.MaxTokens > 0 && estimate(msgs) > int64(c.MaxTokens) {
		return true
	}
	return false
}

// Compactor 返回调用层压缩的 MessageRewriter:历史超过阈值时,
// 把较早的消息摘要为一条 system 记录,保留最近若干条。
//
// 同一会话内做增量摘要缓存:一轮内多次触发时,只对新增部分做
// 增量归并,不重复摘要整个前缀。摘要失败时保守地返回原历史
// (压缩是优化,不是正确性前提)。
func Compactor(m model.ToolCallingChatModel, cfg CompactionConfig) engine.MessageModifier {
	if !cfg.Enabled() {
		return nil
	}
	cache := &summaryCache{entries: map[string]summaryEntry{}}

	return func(ctx context.Context, msgs []*schema.Message) []*schema.Message {
		if !cfg.Over(msgs) {
			return msgs
		}
		cut := len(msgs) - cfg.Keep()
		// 不能从 tool 响应消息处切开(会拆散 assistant 的 tool_call 配对),
		// 向后推进直到边界落在完整轮次上。
		for cut < len(msgs) && msgs[cut].Role == schema.Tool {
			cut++
		}
		if cut <= 0 || cut >= len(msgs) {
			return msgs
		}

		session := runctx.Agent(ctx) + "/" + runctx.Session(ctx)
		prefix := msgs[:cut]
		var summary string
		var err error
		if prev, ok := cache.get(session); ok && prev.prefixLen <= cut {
			// 增量:上次摘要 + 新增部分 → 归并摘要
			delta := append([]*schema.Message{schema.SystemMessage("[已有摘要]\n" + prev.summary)}, msgs[prev.prefixLen:cut]...)
			summary, err = Summarize(ctx, m, delta)
		} else {
			summary, err = Summarize(ctx, m, prefix)
		}
		if err != nil {
			return msgs
		}
		cache.put(session, summaryEntry{prefixLen: cut, summary: summary})

		out := make([]*schema.Message, 0, len(msgs)-cut+1)
		out = append(out, schema.SystemMessage("[早前对话与执行记录摘要]\n"+summary))
		out = append(out, msgs[cut:]...)
		return out
	}
}

// Summarize 把一段对话与执行记录压缩为要点摘要,保留目标、关键事实、
// 已完成操作与未完成事项。会话滚动摘要持久化(agent 包)复用此函数。
func Summarize(ctx context.Context, m model.ToolCallingChatModel, msgs []*schema.Message) (string, error) {
	var sb strings.Builder
	for _, msg := range msgs {
		content := msg.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, content)
	}
	out, err := m.Generate(ctx, []*schema.Message{
		schema.SystemMessage("把以下对话与工具执行记录压缩成要点摘要,保留:用户目标、关键事实与数据、已完成的操作、未完成的事项。丢弃寒暄与过程细节。若含[已有摘要]段,把它与新内容归并为一份摘要。"),
		schema.UserMessage(sb.String()),
	})
	if err != nil {
		return "", err
	}
	return out.Content, nil
}

// SafeCut 返回不拆散 tool-call 配对的切割点(向后推进跳过 tool 消息)。
func SafeCut(msgs []*schema.Message, cut int) int {
	for cut < len(msgs) && cut >= 0 && msgs[cut].Role == schema.Tool {
		cut++
	}
	return cut
}

type summaryEntry struct {
	prefixLen int
	summary   string
}

type summaryCache struct {
	mu      sync.Mutex
	entries map[string]summaryEntry
}

func (c *summaryCache) get(key string) (summaryEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

func (c *summaryCache) put(key string, e summaryEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > 256 { // 粗粒度防泄漏
		c.entries = map[string]summaryEntry{}
	}
	c.entries[key] = e
}
