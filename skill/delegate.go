package skill

// delegate.go:动态委派(single-agent-mode-plan §1.4)——Claude Code Task
// 语义的 agent-kit 等价物。模型运行期把一个可独立完成的子任务写成 task,
// 现场组装隔离子循环执行:适合并行推进多条线索、或中间数据量大不宜进入
// 主上下文的任务("必要时扩展子 agent"的运行期通道;设计期已知的用
// component/graph 声明)。
//
// 治理:深度 1(子面不含 delegate 与交互类工具,防递归裂变与后台任务
// 卡在等人);轮数上限只可收紧不可放宽;并发经信号量封顶;预算/审批经
// ctx 与宿主共账;scope 按 dlg:#N 隔离(进度/去重/轨迹按域分组)。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// DelegateConfig 是动态委派的治理面(agents.delegate)。
type DelegateConfig struct {
	Enabled     bool `yaml:"enabled"`
	MaxRounds   int  `yaml:"max_rounds"`   // 子循环轮数缺省 8;调用参数只可收紧
	MaxParallel int  `yaml:"max_parallel"` // 并发上限缺省 4,超出排队
}

const delegateDesc = "把一个可独立完成的子任务委派给隔离执行体:适合并行推进多条互不依赖的线索、或中间数据量大不宜进入当前对话的任务。子任务只返回最终结果,过程不占用你的上下文。tools 缺省 = 你的工具面(除 delegate 与交互类工具);context: fork 携带当前对话快照。同一轮可发多个 delegate 并行执行。"

// NewDelegate 构造 delegate 内置能力。host 是宿主的原始工具面(未套宿主
// 级门闸——子循环用 applyGates 自套,与 component 同一纪律)。
func NewDelegate(m einomodel.ToolCallingChatModel, host []capability.Capability, cfg DelegateConfig, deps Deps) capability.Capability {
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = 8
	}
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 4
	}
	// 子面候选:剔除交互类(后台任务等人 = 卡死)与过程卡之外全部可用;
	// delegate 自身不在 host 里(装配层后加),深度 1 天然成立。
	pool := map[string]capability.Capability{}
	var names []string
	for _, c := range host {
		meta := c.Meta()
		if hasTagStr(meta.Tags, capability.TagInteractive) {
			continue
		}
		pool[meta.Ref.Name] = c
		names = append(names, meta.Ref.Name)
	}
	sort.Strings(names)

	sem := make(chan struct{}, cfg.MaxParallel)
	var seq atomic.Int64

	params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"task": {Type: schema.String, Required: true,
			Desc: "子任务的完整描述:目标、范围、期望产出,自包含(执行体看不到你的对话)"},
		"tools": {Type: schema.Array, Desc: "限定子任务可用的工具名(缺省全部)",
			ElemInfo: &schema.ParameterInfo{Type: schema.String}},
		"context": {Type: schema.String, Desc: "fresh(缺省,空白起步)| fork(携带当前对话快照)"},
	})
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "builtin", Name: "delegate"},
		Description: delegateDesc,
		Params:      params,
		Risk:        capability.RiskReadonly, // 子面工具各带各的审批闸,入口本身无副作用
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Task    string   `json:"task"`
			Tools   []string `json:"tools"`
			Context string   `json:"context"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Task) == "" {
			return "invalid arguments: task is required (a self-contained description of the sub-task).", nil
		}
		selected := make([]capability.Capability, 0, len(pool))
		if len(args.Tools) == 0 {
			for _, n := range names {
				selected = append(selected, pool[n])
			}
		} else {
			var unknown []string
			for _, n := range args.Tools {
				if c, ok := pool[n]; ok {
					selected = append(selected, c)
				} else {
					unknown = append(unknown, n)
				}
			}
			if len(unknown) > 0 {
				return fmt.Sprintf("invalid arguments: unknown tool(s) %s; available: %s",
					strings.Join(unknown, ", "), strings.Join(names, ", ")), nil
			}
		}

		// 并发闸:同轮多个 delegate 并行,超出上限的排队而非拒绝。
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-sem }()

		ctx = runctx.WithScopePush(ctx, fmt.Sprintf("dlg:#%d", seq.Add(1)))
		ctx = runctx.WithInput(ctx, args.Task)

		gated := applyGates(selected, m, deps)
		runner, err := engine.Build(ctx, "react", &engine.Assembly{
			Model: loop.ReviewModel(m, loop.RepeatBreakReviewer(), loop.FinishReviewer(),
				loop.CheckedReviewer(loop.DeniedCallsCheck)),
			Capabilities: gated,
			MaxSteps:     cfg.MaxRounds,
			Modifier:     loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo + delegatedSection}.Modifier(),
		})
		if err != nil {
			return "", fmt.Errorf("delegate: assemble sub-loop: %w", err)
		}
		var msgs []*schema.Message
		if args.Context == "fork" {
			msgs = loop.ForkMessages(ctx, schema.UserMessage(args.Task))
		} else {
			msgs = []*schema.Message{schema.UserMessage(args.Task)}
		}
		out, err := runner.Generate(ctx, msgs)
		if err != nil {
			return "", err // 轮次终止级(挂起/预算)由上游 turnTerminal 判定穿透
		}
		return out.Content, nil
	})
}

func hasTagStr(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
