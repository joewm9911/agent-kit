package loop

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/runctx"
)

// CompactionConfig 控制上下文压缩,两个阈值任一超过即触发。
type CompactionConfig struct {
	// MaxMessages 触发压缩的消息条数阈值。
	MaxMessages int `yaml:"max_messages" json:"max_messages"`
	// MaxTokens 触发压缩的 token 阈值。优先用供应商回报的真实用量
	// 校准(最近一条 assistant 的 Usage + 其后消息的估算),缺用量时
	// 按字符估算。
	MaxTokens int `yaml:"max_tokens" json:"max_tokens"`
	// KeepRecent 压缩后保留的最近消息条数,默认 MaxMessages/2(或 10)。
	KeepRecent int `yaml:"keep_recent" json:"keep_recent"`
	// Prompt 覆盖摘要的内容策略(保留什么、领域侧重),字面量或
	// {ref: cap://prompt...};缺省用内置。归并指令由框架无条件追加,
	// 不随本字段被覆盖(增量归并是压缩算法的机制部分)。
	Prompt prompt.Value `yaml:"prompt" json:"-"`

	resolvedPrompt string // ResolvePrompt 的产物,装配期填充
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

// ResolvePrompt 在装配期解析摘要提示词引用(锁版本)。未配置时空操作。
func (c *CompactionConfig) ResolvePrompt(ctx context.Context, r prompt.Source) error {
	if c.Prompt.IsZero() {
		return nil
	}
	tpl, err := c.Prompt.Resolve(ctx, r)
	if err != nil {
		return fmt.Errorf("compaction prompt: %w", err)
	}
	c.resolvedPrompt = tpl.Text
	return nil
}

const defaultSummarizePrompt = `把以下对话与工具执行记录压缩成要点摘要,保留:用户目标、关键事实与数据、已完成的操作、未完成的事项。丢弃寒暄与过程细节。`

// mergeClause 是框架追加的归并指令:增量归并是压缩算法的机制部分,
// 不随内容策略被配置覆盖。
const mergeClause = `若输入含 [已有摘要] 段,把它与新内容归并为一份摘要,不要丢失其中的既有要点。`

func (c CompactionConfig) summarizePrompt() string {
	p := c.resolvedPrompt
	if p == "" {
		p = defaultSummarizePrompt
	}
	return p + "\n" + mergeClause
}

// Over 报告消息集是否超过任一阈值。token 维度优先用真实用量校准。
func (c CompactionConfig) Over(msgs []*schema.Message) bool {
	if c.MaxMessages > 0 && len(msgs) > c.MaxMessages {
		return true
	}
	if c.MaxTokens > 0 && contextTokens(msgs) > int64(c.MaxTokens) {
		return true
	}
	return false
}

// contextTokens 估算消息集的 token 量:从尾部找最近一条带供应商用量
// 的 assistant 消息——它的 TotalTokens 即当时全上下文的真实大小,
// 其后的消息再做字符估算补齐。找不到用量时整体退化为字符估算。
func contextTokens(msgs []*schema.Message) int64 {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == schema.Assistant && m.ResponseMeta != nil && m.ResponseMeta.Usage != nil &&
			m.ResponseMeta.Usage.TotalTokens > 0 {
			return int64(m.ResponseMeta.Usage.TotalTokens) + estimate(msgs[i+1:])
		}
	}
	return estimate(msgs)
}

// Compactor 返回调用层压缩的 MessageRewriter:历史超过阈值时,
// 把较早的消息摘要为一条 system 记录,保留最近若干条。
//
// 压缩是低频的一次性事件,不是持续重写:一旦压缩,后续调用复用
// 缓存的(切割点, 摘要)重建视图——前缀稳定,供应商的 prompt cache
// 在两次压缩之间持续命中;直到视图再次超阈值才做增量归并重压。
// 摘要失败时保守地返回原历史(压缩是优化,不是正确性前提)。
//
// 锚定保护:视图头部除摘要外常驻本段首条用户消息原文——多次归并后
// "最初的任务"不漂移。缓存按 (agent, session, 执行域) 分键,并行
// 调用同一 component 不互相抖动。
func Compactor(m model.ToolCallingChatModel, cfg CompactionConfig) engine.MessageModifier {
	if !cfg.Enabled() {
		return nil
	}
	cache := &summaryCache{entries: map[string]summaryEntry{}}

	return func(ctx context.Context, msgs []*schema.Message) []*schema.Message {
		session := runctx.Agent(ctx) + "\x1f" + runctx.Session(ctx)
		if scope := runctx.Scope(ctx); scope != "" {
			session += "\x1f" + scope
		}

		if prev, ok := cache.get(session); ok && prev.valid(msgs) {
			view := prev.view(msgs)
			if !cfg.Over(view) {
				return view // 稳定前缀:不重切,cache 持续命中
			}
			// 视图再次超阈值:增量归并(旧摘要 + 新增部分)后重切
			cut := SafeCut(msgs, len(msgs)-cfg.Keep())
			if cut <= prev.prefixLen || cut >= len(msgs) {
				return view
			}
			delta := append([]*schema.Message{schema.SystemMessage("[已有摘要]\n" + prev.summary)}, msgs[prev.prefixLen:cut]...)
			summary, err := Summarize(ctx, m, cfg, delta)
			if err != nil {
				return view
			}
			next := summaryEntry{prefixLen: cut, summary: summary, boundary: fingerprint(msgs[cut-1])}
			cache.put(session, next)
			logCompaction("incremental", session, cut, len(msgs)-cut, summary)
			return next.view(msgs)
		}

		if !cfg.Over(msgs) {
			return msgs
		}
		// 首次压缩:切割点不拆散 tool_call 配对
		cut := SafeCut(msgs, len(msgs)-cfg.Keep())
		if cut <= 0 || cut >= len(msgs) {
			return msgs
		}
		summary, err := Summarize(ctx, m, cfg, msgs[:cut])
		if err != nil {
			return msgs
		}
		entry := summaryEntry{prefixLen: cut, summary: summary, boundary: fingerprint(msgs[cut-1])}
		cache.put(session, entry)
		logCompaction("initial", session, cut, len(msgs)-cut, summary)
		return entry.view(msgs)
	}
}

// logCompaction 打压缩事件的结构化日志:排查"agent 忘了事"时,
// 何时切、切了多少、摘要多长一目了然。
func logCompaction(kind, session string, cut, kept int, summary string) {
	slog.Info("context compacted", "kind", kind,
		"session", strings.ReplaceAll(session, "\x1f", "/"),
		"cut", cut, "kept", kept, "summary_runes", len([]rune(summary)))
}

// valid 校验缓存与当前消息列表对齐:切割点未越界,且边界消息未变
// (跨轮织入的历史形状可能变化,错位则放弃缓存重新评估)。
func (e summaryEntry) valid(msgs []*schema.Message) bool {
	return e.prefixLen > 0 && e.prefixLen < len(msgs) && fingerprint(msgs[e.prefixLen-1]) == e.boundary
}

// view 用缓存重建织入视图:[摘要] + [锚定的首条用户消息原文] +
// 切割点之后的原始消息。
func (e summaryEntry) view(msgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs)-e.prefixLen+2)
	out = append(out, schema.SystemMessage("[早前对话与执行记录摘要]\n"+e.summary))
	for _, m := range msgs[:e.prefixLen] {
		if m.Role == schema.User && m.Content != "" {
			out = append(out, m) // 锚定:被摘要覆盖的最初任务原文常驻
			break
		}
	}
	return append(out, msgs[e.prefixLen:]...)
}

// fingerprint 取消息的轻量指纹(角色+内容前缀),用于缓存对齐校验。
func fingerprint(m *schema.Message) string {
	c := m.Content
	if len(c) > 64 {
		c = c[:64]
	}
	return string(m.Role) + "|" + c
}

// summarizeClipLen 是进入摘要器的单条消息上限(rune)。太小会让摘要器
// 只见残片("保留关键事实"无从保起),太大浪费窗口。
const summarizeClipLen = 2000

// Summarize 把一段对话与执行记录压缩为要点摘要。内容策略可经
// cfg.Prompt 配置(领域自定义"保留清单"),归并指令由框架追加。
// 会话滚动摘要持久化(agent 包)复用此函数。
func Summarize(ctx context.Context, m model.ToolCallingChatModel, cfg CompactionConfig, msgs []*schema.Message) (string, error) {
	var sb strings.Builder
	for _, msg := range msgs {
		content := msg.Content
		if r := []rune(content); len(r) > summarizeClipLen {
			content = string(r[:summarizeClipLen]) + "..."
		}
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, content)
	}
	out, err := m.Generate(ctx, []*schema.Message{
		schema.SystemMessage(cfg.summarizePrompt()),
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
	boundary  string // 切割点前一条消息的指纹,用于跨调用对齐校验
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
