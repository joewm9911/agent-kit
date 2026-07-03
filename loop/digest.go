package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
)

// TagRawResult 标记一个能力的结果不参与消化(结果本身就是给模型的
// 受控输出,如 read_result 的分页;供给方也可用它显式豁免)。
const TagRawResult = "result:raw"

// ResultStore 是一轮运行内的工具结果暂存:被消化的原始全文存在这里,
// 模型觉得摘要不够时用 read_result 分页取回。由 agent 每次运行装入
// ctx,随轮次结束丢弃——消化默认省上下文,需要时可追溯。
type ResultStore struct {
	mu      sync.Mutex
	seq     int
	entries map[string]storedResult
	bytes   int
}

type storedResult struct {
	tool string
	text string
}

// storeMaxBytes 是单轮暂存的总量上限,超出后最早的条目被丢弃语义
// 简化为:不再接收新条目(返回空 id,消化附言退化为纯截断提示)。
const storeMaxBytes = 4 << 20 // 4MB

// NewResultStore 创建一轮运行的结果暂存。
func NewResultStore() *ResultStore {
	return &ResultStore{entries: map[string]storedResult{}}
}

// Put 存入一条原始结果,返回取回 id;超过总量上限时返回空串。
func (s *ResultStore) Put(toolName, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bytes+len(text) > storeMaxBytes {
		return ""
	}
	s.seq++
	id := fmt.Sprintf("r%d", s.seq)
	s.entries[id] = storedResult{tool: toolName, text: text}
	s.bytes += len(text)
	return id
}

// Get 取回一条原始结果。
func (s *ResultStore) Get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	return e.text, ok
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

// digestMaxInput 是送入消化模型的原文上限(rune),防止消化本身打爆窗口。
const digestMaxInput = 20000

const digestSystem = `你是结果消化器。把工具返回的原始结果压缩为与当前任务相关的要点:
- 保留关键数据的原文:ID、时间戳、错误码、数字、路径、名称,不要改写;
- 只做提取与压缩,不要添加任何推断或建议;
- 与任务无关的部分用一句话概括其存在即可;
- 输出不超过 800 字。`

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
	runes := []rune(out)
	if len(runes) <= d.over {
		return out
	}
	store := resultStoreFrom(ctx)
	if store == nil {
		return out
	}
	name := d.inner.Meta().Ref.Name
	id := store.Put(name, out)

	clipped := out
	if len(runes) > digestMaxInput {
		clipped = string(runes[:digestMaxInput]) + "\n...[消化输入已截断]"
	}
	task := runctx.Input(ctx)
	if task == "" {
		task = "(未知,保守保留通用要点)"
	}
	sum, err := d.m.Generate(ctx, []*schema.Message{
		schema.SystemMessage(digestSystem),
		schema.UserMessage(fmt.Sprintf("当前任务:%s\n\n工具 %s 的原始结果:\n%s", task, name, clipped)),
	})
	if err != nil {
		return out // 消化失败退回原样,截断闸兜底
	}
	pointer := "全文未能暂存(本轮暂存已满)"
	if id != "" {
		pointer = fmt.Sprintf("全文已存为 %s,需要细节可用 read_result(id=%q, offset=N) 分页查看", id, id)
	}
	return fmt.Sprintf("[结果已消化:原始 %d 字符;%s]\n%s", len(runes), pointer, sum.Content)
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
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "context", Name: "read_result"},
		Description: "分页读取被消化工具结果的原文。仅当摘要信息不足时使用,按 offset 逐页推进。",
		Params:      params,
		Tags:        []string{TagRawResult}, // 自身分页输出不再消化
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		store := resultStoreFrom(ctx)
		if store == nil {
			return "本轮没有可取回的结果暂存。", nil
		}
		var args struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &args)
		text, ok := store.Get(args.ID)
		if !ok {
			return fmt.Sprintf("结果 %q 不存在或已随轮次结束丢弃。", args.ID), nil
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
