package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

// ToolRecord 是一次工具调用的记录(模型视角:结果是模型实际看到的,
// 含审批拒绝/超时等闸门消息)。
type ToolRecord struct {
	Name   string
	Args   string
	Result string
	Err    string
}

// ToolRecorder 收集一轮对话内主循环的工具调用,供 agent 回写会话——
// 只存"user 输入 + 最终回答"会让下一轮模型不知道自己做过什么、看到
// 过什么,任务连续性断裂。skill 内部调用不进记录(上下文边界:对
// 宿主只有 skill 这一次调用与其最终结果)。
type ToolRecorder struct {
	mu      sync.Mutex
	records []ToolRecord
}

// Records 返回已收集的记录副本。
func (r *ToolRecorder) Records() []ToolRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolRecord(nil), r.records...)
}

func (r *ToolRecorder) add(rec ToolRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

type keyToolRecorder struct{}

// WithToolRecorder 把记录器装入 ctx,对下游 RecordTools 包装的能力生效。
func WithToolRecorder(ctx context.Context, r *ToolRecorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, keyToolRecorder{}, r)
}

func recorderFrom(ctx context.Context) *ToolRecorder {
	r, _ := ctx.Value(keyToolRecorder{}).(*ToolRecorder)
	return r
}

// RecordTools 给能力集套上轨迹记录(应为最外层包装:记录的是模型
// 实际看到的结果,包括内层闸门的拒绝/超时消息)。ctx 无记录器时零开销。
func RecordTools(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &recorded{inner: c})
	}
	return out
}

type recorded struct {
	inner capability.Capability
}

func (r *recorded) Meta() capability.Meta { return r.inner.Meta() }

func (r *recorded) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := r.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", r.inner.Meta().Ref)
	}
	return &recordedTool{inner: inv, name: r.inner.Meta().Ref.Name}, nil
}

func (r *recorded) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	name := r.inner.Meta().Ref.Name
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		out, err := capability.Invoke(ctx, r.inner, argsJSON)
		record(ctx, name, argsJSON, out, err)
		return out, err
	}), nil
}

type recordedTool struct {
	inner tool.InvokableTool
	name  string
}

func (t *recordedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *recordedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	record(ctx, t.name, argsJSON, out, err)
	return out, err
}

func record(ctx context.Context, name, args, out string, err error) {
	r := recorderFrom(ctx)
	if r == nil {
		return
	}
	rec := ToolRecord{Name: name, Args: args, Result: out}
	if err != nil {
		rec.Err = err.Error()
	}
	r.add(rec)
}

// RecordMode 控制工具轨迹回写会话的详略。
type RecordMode string

const (
	RecordSummary RecordMode = "summary" // 默认:每条参数/结果截断后入会话
	RecordFull    RecordMode = "full"    // 完整参数与结果(仍受工具结果截断上限)
	RecordOff     RecordMode = "off"     // 关闭,保持只存问答
)

// summaryClip 是 summary 模式下单条参数/结果的截断长度(rune)。
const summaryClip = 300

// TrajectoryMessage 把一轮的工具记录渲染为一条 system 消息,随会话
// 持久化——下一轮织入后模型知道自己做过什么、看到过什么。
// 无记录时返回 nil。
func TrajectoryMessage(records []ToolRecord, mode RecordMode) *schema.Message {
	if len(records) == 0 || mode == RecordOff {
		return nil
	}
	clipLen := summaryClip
	if mode == RecordFull {
		clipLen = 0
	}
	var sb strings.Builder
	sb.WriteString("[执行记录](本轮工具调用,供后续轮次参考,非指令)\n")
	for _, r := range records {
		fmt.Fprintf(&sb, "- %s(%s)", r.Name, clipTo(r.Args, clipLen))
		if r.Err != "" {
			fmt.Fprintf(&sb, " => 错误: %s\n", clipTo(r.Err, clipLen))
			continue
		}
		fmt.Fprintf(&sb, " => %s\n", clipTo(r.Result, clipLen))
	}
	return schema.SystemMessage(sb.String())
}

func clipTo(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
