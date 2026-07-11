package loop

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/runtime/engine"
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

	// ToolClearOver 启用旧轮工具结果清理(借 eino ADK ToolReduction 的
	// Clear 阶段设计):保护窗之外的 tool 消息,内容超过该 rune 数即
	// 替换为占位。比整段摘要便宜一个量级——纯字符串操作、零模型调用;
	// 先清理后压缩,摘要输入也更小。0 = 关闭。已消化(digest)的结果
	// 带取回指针,跳过不清。
	ToolClearOver int `yaml:"tool_clear_over" json:"tool_clear_over"`
	// ToolClearKeep 清理保护窗:最近 N 条消息不清,默认取 KeepRecent
	// (未配压缩时默认 8)。
	ToolClearKeep int `yaml:"tool_clear_keep" json:"tool_clear_keep"`

	resolvedPrompt string // ResolvePrompt 的产物,装配期填充
}

// Enabled 报告压缩(或工具结果清理)是否启用。
func (c CompactionConfig) Enabled() bool {
	return c.MaxMessages > 0 || c.MaxTokens > 0 || c.ToolClearOver > 0
}

func (c CompactionConfig) toolClearKeep() int {
	if c.ToolClearKeep > 0 {
		return c.ToolClearKeep
	}
	if k := c.Keep(); k > 0 {
		return k
	}
	return 8
}

const toolClearedPrefix = "[工具结果已清理:"

// clearOldToolResults 把保护窗之外的超长 tool 消息内容替换为占位。
// 只改内容不删消息——tool_call 配对不受影响,协议安全;确定性且幂等
// (占位短于阈值,已消化结果按指纹跳过),每轮从 store 重建视图后重清
// 得到相同结果,不抖动摘要缓存的边界指纹。
func clearOldToolResults(msgs []*schema.Message, over, keep int) []*schema.Message {
	if over <= 0 || len(msgs) <= keep {
		return msgs
	}
	limit := len(msgs) - keep
	var out []*schema.Message // copy-on-write:未清理时零分配
	for i, m := range msgs {
		if i >= limit || m.Role != schema.Tool {
			if out != nil {
				out = append(out, m)
			}
			continue
		}
		runes := []rune(m.Content)
		if len(runes) <= over ||
			strings.HasPrefix(m.Content, toolClearedPrefix) ||
			strings.HasPrefix(m.Content, "[结果过长且消化失败") || // digest 失败指针:自带 read_result 取回指引,清掉反而断路
			strings.Contains(m.Content, "[结果已消化") {
			if out != nil {
				out = append(out, m)
			}
			continue
		}
		if out == nil {
			out = append([]*schema.Message{}, msgs[:i]...)
		}
		clone := *m
		clone.Content = fmt.Sprintf("%s原始 %d 字符。该结果已由此后的对话消化;如需原始数据,重新调用相应工具]",
			toolClearedPrefix, len(runes))
		out = append(out, &clone)
	}
	if out == nil {
		return msgs
	}
	return out
}

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

const defaultSummarizePrompt = `Compress the following conversation and tool-execution records into a bullet-point summary, keeping: the user's goal, key facts and data, actions already completed, and outstanding items. Drop pleasantries and process details.`

// mergeClause 是框架追加的归并指令:增量归并是压缩算法的机制部分,
// 不随内容策略被配置覆盖。
const mergeClause = `If the input contains an [Existing summary] section, merge it with the new content into a single summary without losing any of its existing points.`

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
		// 阶段0:旧轮工具结果清理(零成本,先于摘要——摘要输入更小)。
		msgs = clearOldToolResults(msgs, cfg.ToolClearOver, cfg.toolClearKeep())

		session := runctx.Agent(ctx) + "\x1f" + runctx.Session(ctx)
		if scope := runctx.Scope(ctx); scope != "" {
			session += "\x1f" + scope
		}

		if prev, ok := cache.get(session); ok && prev.valid(msgs) {
			view := prev.view(msgs)
			if !cfg.Over(view) {
				return view // 稳定前缀:不重切,cache 持续命中
			}
			// 视图再次超阈值:增量归并(旧摘要 + 新增部分)后重切。
			// 泄压阀同样作用于增量路径:否则会话一旦有过摘要,"最近即膨胀源"
			// 的场景每次都走这里,保留窗恒超预算、阀永远够不着(稳态失效)。
			cut := pressureCut(msgs, SafeCut(msgs, len(msgs)-cfg.Keep()), cfg.MaxTokens)
			if cut <= prev.prefixLen || cut >= len(msgs) {
				return view
			}
			delta := append([]*schema.Message{schema.SystemMessage("[Existing summary]\n" + prev.summary)}, msgs[prev.prefixLen:cut]...)
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
		// U5.1 泄压阀:keep_recent 是硬保护,但"最近即膨胀源"时(保留窗
		// 本身就超 token 预算)会让压缩后仍然超限、逼近上下文上限。仅在
		// 真顶到 MaxTokens 时突破保护、把切割点前移,直到最近窗口装得下。
		cut = pressureCut(msgs, cut, cfg.MaxTokens)
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
	out = append(out, schema.SystemMessage("[Earlier conversation and execution summary]\n"+e.summary))
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
	out, err := observedGenerate(ctx, "compaction/summarize", func(ctx context.Context, ms []*schema.Message) (*schema.Message, error) {
		return m.Generate(ctx, ms)
	}, []*schema.Message{
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

// minKeepFloor 是泄压阀下探的保留下限:再顶也至少留这么多条最近消息。
const minKeepFloor = 2

// pressureCut 是 U5.1 泄压阀:标准切割(keep_recent)后若保留窗
// msgs[cut:] 的 token 估算仍超预算(留 1/4 给摘要),把切割点前移(不拆
// tool 配对),直到装得下或触到 minKeepFloor 下限。maxTokens<=0 不介入。
func pressureCut(msgs []*schema.Message, cut, maxTokens int) int {
	if maxTokens <= 0 {
		return cut
	}
	// 消息数少于 keep_recent 时上游的 len(msgs)-Keep() 为负(SafeCut 原样
	// 放行)——钳到 0,否则 msgs[cut:] 直接越界 panic(实测:max_tokens 配置
	// + 短而肥的历史,一次大段粘贴即可打崩无 recover 的 IM 路径)。
	if cut < 0 {
		cut = 0
	}
	budget := int64(maxTokens) * 3 / 4
	for cut < len(msgs)-minKeepFloor && estimate(msgs[cut:]) > budget {
		next := SafeCut(msgs, cut+1)
		if next <= cut || next >= len(msgs)-minKeepFloor {
			break
		}
		cut = next
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
