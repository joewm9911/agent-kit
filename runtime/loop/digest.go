package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
)

// TagRawResult 别名 core 的跨层常量(见 capability.TagRawResult);
// 语义:能力的结果不参与消化(结果本身就是给模型的
// 受控输出,如 read_result 的分页;供给方也可用它显式豁免)。
const TagRawResult = capability.TagRawResult

const rsep = "\x1f"

// 结果暂存的后端由装配层构造并注入(见 NewResultStore):agent 每次运行
// 持有自己的后端、经 ctx 下发,不读任何全局单例——同进程多 agent 各持
// 各的 result 后端,互不覆盖。外置到 redis 后,被消化的原文可跨挂起/恢复、
// 跨副本取回。

// ResultStore 是一轮运行的工具结果暂存句柄:被消化的原始全文存进后端 KV,
// 模型觉得摘要不够时用 read_result 分页取回。键按 (agent, session) 作用域,
// 共享后端下不跨会话碰撞;序号经后端原子自增,跨副本/恢复不撞 id。bytes
// 是本句柄的软性准入计数(跨副本近似即可,只是防单轮暂存爆量的安全阀)。
type ResultStore struct {
	mu    sync.Mutex
	kv    store.KV
	ttl   time.Duration
	bytes int
}

// storeMaxBytes 是单轮暂存的总量软上限,超出后不再接收新条目(返回空
// id,消化附言退化为纯截断提示)。
const storeMaxBytes = 4 << 20 // 4MB

// NewResultStore 创建一轮运行的结果暂存句柄,绑定注入的后端与保留时长。
// kv 为 nil 时返回 nil(该 agent 未配置结果暂存,digest 退化为纯截断)。
func NewResultStore(kv store.KV, ttl time.Duration) *ResultStore {
	if kv == nil {
		return nil
	}
	return &ResultStore{kv: kv, ttl: ttl}
}

// rscope 取 (agent, session) 作为键命名空间,隔离并发会话与多副本。
func rscope(ctx context.Context) string {
	return runctx.Agent(ctx) + rsep + runctx.Session(ctx)
}

// Put 存入一条原始结果,返回取回 id;超过总量软上限时返回空串。
func (s *ResultStore) Put(ctx context.Context, toolName, text string) string {
	s.mu.Lock()
	if s.bytes+len(text) > storeMaxBytes {
		s.mu.Unlock()
		return ""
	}
	s.bytes += len(text)
	s.mu.Unlock()

	scope := rscope(ctx)
	n, err := s.nextSeq(ctx, scope)
	if err != nil {
		slog.Warn("result store seq failed, degrading digest pointer", "err", err)
		return "" // 诚实降级:不发幻影 id(消化附言改说"全文未能暂存")
	}
	id := fmt.Sprintf("r%d", n)
	if err := s.kv.Update(ctx, scope+rsep+id, func(_ []byte, _ bool) ([]byte, error) {
		return []byte(text), nil
	}, s.ttl); err != nil {
		// redis 写失败绝不能吞:否则 Put 照样交出 id,消化附言宣称
		// "全文已存为 r1",read_result 一查"不存在"——生产实测的幻影 id。
		slog.Warn("result store put failed, degrading digest pointer", "id", id, "err", err)
		return ""
	}
	return id
}

// PutDeliver 存入一份交付物原文,返回取回 id(d<N>,后端持久序,跨轮
// 唯一);后端失败返回空串,由 sink 分配轮内降级 id。与 Put 共享单轮
// 总量软上限。
func (s *ResultStore) PutDeliver(ctx context.Context, text string) string {
	s.mu.Lock()
	if s.bytes+len(text) > storeMaxBytes {
		s.mu.Unlock()
		return ""
	}
	s.bytes += len(text)
	s.mu.Unlock()

	scope := rscope(ctx)
	var n int
	if err := s.kv.Update(ctx, scope+rsep+"#dseq", func(old []byte, ok bool) ([]byte, error) {
		if ok {
			n, _ = strconv.Atoi(string(old))
		}
		n++
		return []byte(strconv.Itoa(n)), nil
	}, s.ttl); err != nil {
		slog.Warn("deliver store seq failed, degrading to turn-local id", "err", err)
		return ""
	}
	id := fmt.Sprintf("d%d", n)
	if err := s.kv.Update(ctx, scope+rsep+id, func(_ []byte, _ bool) ([]byte, error) {
		return []byte(text), nil
	}, s.ttl); err != nil {
		slog.Warn("deliver store put failed, degrading to turn-local id", "id", id, "err", err)
		return ""
	}
	return id
}

// nextSeq 经后端原子自增取下一个序号,保证跨副本/恢复不撞 id。后端失败
// 必须上抛:吞掉的话 n 恒为 1,坏后端下每轮都发同一个取不回的 "r1"。
func (s *ResultStore) nextSeq(ctx context.Context, scope string) (int, error) {
	var n int
	err := s.kv.Update(ctx, scope+rsep+"#seq", func(old []byte, ok bool) ([]byte, error) {
		if ok {
			n, _ = strconv.Atoi(string(old))
		}
		n++
		return []byte(strconv.Itoa(n)), nil
	}, s.ttl)
	return n, err
}

// Get 取回一条原始结果。后端读错误与"不存在"必须可区分:redis 抖动时把
// 有效 id 报成"不存在",模型会永久放弃一个其实取得回的结果。
func (s *ResultStore) Get(ctx context.Context, id string) (string, bool, error) {
	b, ok, err := s.kv.Get(ctx, rscope(ctx)+rsep+id)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	return string(b), true, nil
}

// List 返回本 (agent, session) 作用域下可取回的 id(按序,上限 10,
// 排除内部 #seq 计数键)。read_result miss 时给模型自纠线索。
func (s *ResultStore) List(ctx context.Context) []string {
	keys, err := s.kv.Scan(ctx, rscope(ctx)+rsep)
	if err != nil {
		return nil
	}
	var ids []string
	for _, k := range keys {
		id := k[strings.LastIndex(k, rsep)+len(rsep):]
		if strings.HasPrefix(id, "r") || strings.HasPrefix(id, "d") {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.Atoi(ids[i][1:])
		b, _ := strconv.Atoi(ids[j][1:])
		return a < b
	})
	if len(ids) > 10 {
		ids = ids[len(ids)-10:] // 留最近的
	}
	return ids
}

type keyResultStore struct{}

// WithResultStore 把结果暂存装入 ctx。
func WithResultStore(ctx context.Context, s *ResultStore) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, keyResultStore{}, s)
}

func resultStoreFrom(ctx context.Context) *ResultStore {
	s, _ := ctx.Value(keyResultStore{}).(*ResultStore)
	return s
}

// resultIDRe 匹配取回 id 的规范形态(r+序号)。
var resultIDRe = regexp.MustCompile(`[rd]\d+`)

// digestMaxInput 是送入消化模型的原文上限(rune),防止消化本身打爆窗口。
const digestMaxInput = 20000

const digestSystem = `You are a result digester. Compress the raw tool output into the points relevant to the current task:
- keep key data verbatim: IDs, timestamps, error codes, numbers, paths, names — do not rewrite them;
- extract and compress only; do not add any inference or suggestion;
- for parts unrelated to the task, note their existence in one sentence;
- output at most 800 characters.`

// DigestResults 给能力集套上大结果消化(Ring 0):结果超过 over(rune)
// 时,全文存入 run 级暂存,由模型带着当前任务提取要点,摘要+取回指针
// 入上下文——搜索、捞日志等大数据量工具不再污染调用方上下文。
//
// over<=0 关闭;带 TagRawResult 或 TagInteractive 的能力豁免;ctx 无
// 暂存(直接以库方式调用)或消化失败时退化为原样返回(下游仍有截断
// 闸兜底)。消化模型调用计入调用方会话预算(m 应为已包装的模型)。
func DigestResults(caps []capability.Capability, m model.ToolCallingChatModel, over int) []capability.Capability {
	if over <= 0 || m == nil {
		return caps
	}
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		tags := c.Meta().Tags
		if hasTag(tags, TagRawResult) || hasTag(tags, capability.TagInteractive) {
			out = append(out, c)
			continue
		}
		out = append(out, &digested{inner: c, m: m, over: over})
	}
	return out
}

type digested struct {
	inner capability.Capability
	m     model.ToolCallingChatModel
	over  int
}

func (d *digested) Meta() capability.Meta { return d.inner.Meta() }

func (d *digested) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := d.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", d.inner.Meta().Ref)
	}
	return &digestedTool{inner: inv, d: d}, nil
}

func (d *digested) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		out, err := capability.Invoke(ctx, d.inner, argsJSON)
		if err != nil {
			return out, err
		}
		return d.digest(ctx, out), nil
	}), nil
}

type digestedTool struct {
	inner tool.InvokableTool
	d     *digested
}

func (t *digestedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx) // 消化对模型透明
}

func (t *digestedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		return out, err
	}
	return t.d.digest(ctx, out), nil
}

// digest 执行消化:入暂存 → 模型提取要点 → 摘要+指针。任何一步不
// 具备条件都原样返回,让下游截断闸兜底(消化是优化,不是正确性前提)。
func (d *digested) digest(ctx context.Context, out string) string {
	// 交付物标记行豁免消化改写:剥出首行标记,消化其余,标记回贴摘要
	// 头部——原文在暂存里完好,#dN 引用链路不能因消化而断。
	var deliverMark string
	if strings.HasPrefix(out, "[交付物#") {
		if i := strings.IndexByte(out, '\n'); i > 0 {
			deliverMark, out = out[:i+1], out[i+1:]
		}
	}
	restore := func(s string) string { return deliverMark + s }
	runes := []rune(out)
	if len(runes) <= d.over {
		return restore(out)
	}
	rs := resultStoreFrom(ctx)
	if rs == nil {
		return restore(out)
	}
	name := d.inner.Meta().Ref.Name
	id := rs.Put(ctx, name, out)

	clipped := out
	if len(runes) > digestMaxInput {
		clipped = string(runes[:digestMaxInput]) + "\n...[消化输入已截断]"
	}
	task := runctx.Input(ctx)
	if task == "" {
		task = "(未知,保守保留通用要点)"
	}
	sum, err := observedGenerate(ctx, "digest/"+name, func(ctx context.Context, ms []*schema.Message) (*schema.Message, error) {
		return d.m.Generate(ctx, ms)
	}, []*schema.Message{
		schema.SystemMessage(digestSystem),
		schema.UserMessage(fmt.Sprintf("当前任务:%s\n\n工具 %s 的原始结果:\n%s", task, name, clipped)),
	})
	if err != nil {
		// 消化失败但全文已在暂存里:指针绝不能跟着摘要一起丢——否则下游
		// 截断闸只说"请缩小查询范围",模型只能瞎猜 id(Ark 生产实测:
		// 106778 字符结果消化失败 → 截断无 id → 模型猜 r1 → 读不到)。
		// 确定性降级:开头片段 + 指针,不再需要模型。
		if id != "" {
			head := runes
			if len(head) > d.over {
				head = head[:d.over]
			}
			return restore(fmt.Sprintf("[结果过长且消化失败:原始 %d 字符,以下仅为开头片段;全文已存为 %s,可用 read_result(id=%q, offset=N) 分页查看]\n%s",
				len(runes), id, id, string(head)))
		}
		return restore(out) // 暂存也没成:退回原样,截断闸兜底
	}
	pointer := "全文未能暂存(本轮暂存已满或后端不可用)"
	if id != "" {
		pointer = fmt.Sprintf("全文已存为 %s,需要细节可用 read_result(id=%q, offset=N) 分页查看", id, id)
	}
	return restore(fmt.Sprintf("[结果已消化:原始 %d 字符;%s]\n%s", len(runes), pointer, sum.Content))
}

// readResultPage 是 read_result 单页返回的 rune 数。
const readResultPage = 3000

// ReadResult 构造内置的原文取回工具:配合结果消化使用,按 id 与
// offset 分页读取被消化结果的原文。启用消化的工具面应同时挂载它。
func ReadResult() capability.Capability {
	params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"id":     {Type: schema.String, Desc: "消化附言中给出的结果 id,如 r1", Required: true},
		"offset": {Type: schema.Integer, Desc: "起始字符位置,默认 0"},
	})
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "read_result"},
		Description: "分页读取被消化工具结果的原文。仅当摘要信息不足时使用,按 offset 逐页推进。",
		Risk:        capability.RiskReadonly,
		Params:      params,
		Tags:        []string{TagRawResult}, // 自身分页输出不再消化
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		rs := resultStoreFrom(ctx)
		if rs == nil {
			return "本轮没有可取回的结果暂存。", nil
		}
		var args struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &args)
		// id 归一化:模型实测会传 " r1"/"R1"/"结果r1" 这类脏形态,
		// 逐字节比对全部落空。抽出规范形态再查。
		if m := resultIDRe.FindString(strings.ToLower(args.ID)); m != "" {
			args.ID = m
		}
		text, ok, gerr := rs.Get(ctx, args.ID)
		if gerr != nil {
			// 后端抖动 ≠ 不存在:引导模型稍后重试同一 id,而不是永久放弃。
			return fmt.Sprintf("结果暂存后端暂时读取失败(%q 可能仍存在),请稍后重试 read_result。", args.ID), nil
		}
		if !ok {
			// 顺带回报本会话真实可取回的 id:模型拿错 id 时能立即自纠,
			// 排障时也一眼可见"存了什么 vs 查了什么"。
			if ids := rs.List(ctx); len(ids) > 0 {
				return fmt.Sprintf("结果 %q 不存在或已随轮次结束丢弃。当前会话可取回的 id:%s。", args.ID, strings.Join(ids, ", ")), nil
			}
			return fmt.Sprintf("结果 %q 不存在或已随轮次结束丢弃(当前会话没有任何暂存结果)。", args.ID), nil
		}
		runes := []rune(text)
		if args.Offset < 0 || args.Offset >= len(runes) {
			return fmt.Sprintf("offset 超界:全文共 %d 字符。", len(runes)), nil
		}
		end := args.Offset + readResultPage
		if end > len(runes) {
			end = len(runes)
		}
		return fmt.Sprintf("[%d-%d / 共 %d 字符]\n%s", args.Offset, end, len(runes), string(runes[args.Offset:end])), nil
	})
}
