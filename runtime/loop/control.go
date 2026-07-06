package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

// ErrInterrupted 表示运行被用户主动叫停。
type ErrInterrupted struct{}

func (*ErrInterrupted) Error() string { return "run interrupted by user" }

// TurnTerminal 标记轮次终止级错误(穿透工具错误兜底,见 engine)。
func (*ErrInterrupted) TurnTerminal() {}

// ControlState 是会话级的运行控制:让运行中的循环可被叫停(interrupt)
// 与驾驶(steer)。没有它,"停,别做了"只能排在同会话串行队列后面——
// 等任务做完才被读到,为时已晚。
//
// 检查点在工具执行边界(Ring 0 的切面,不依赖引擎配合):
//   - 中断:下一次工具调用前生效,取消信号同时传播给运行 ctx,
//     终止进行中的模型调用与并行分支;
//   - 插话:追加到下一个工具结果尾部,模型在下一次观察时读到。
type ControlState struct {
	mu          sync.Mutex
	interrupted bool
	queue       []string
	cancel      context.CancelFunc // 当前轮的取消函数
}

// Interrupt 叫停当前运行:置位标志并取消当前轮 ctx。
func (c *ControlState) Interrupt() {
	c.mu.Lock()
	c.interrupted = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Steer 注入一条用户插话,随下一个工具结果送达模型。
func (c *ControlState) Steer(msg string) {
	if msg == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, msg)
}

// Interrupted 报告当前是否处于中断状态。
func (c *ControlState) Interrupted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interrupted
}

// BeginTurn 在一轮开始时复位中断标志并绑定本轮取消函数。
// 插话队列不清空:上一轮没来得及送达的插话在新一轮开头补送。
func (c *ControlState) BeginTurn(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interrupted = false
	c.cancel = cancel
}

// EndTurn 解绑取消函数。
func (c *ControlState) EndTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = nil
}

func (c *ControlState) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.queue
	c.queue = nil
	return out
}

type keyControl struct{}

// WithControl 把会话控制装入 ctx。
func WithControl(ctx context.Context, c *ControlState) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, keyControl{}, c)
}

func controlFrom(ctx context.Context) *ControlState {
	c, _ := ctx.Value(keyControl{}).(*ControlState)
	return c
}

// ControlTools 给能力集套上运行控制切面。应位于审批闸门之外
// (中断时连批准都不再询问)、轨迹记录之内(插话是模型看到的内容,
// 应入记录)。ctx 无控制态时零开销。
func ControlTools(caps []capability.Capability) []capability.Capability {
	out := make([]capability.Capability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &controlled{inner: c})
	}
	return out
}

type controlled struct {
	inner capability.Capability
}

func (c *controlled) Meta() capability.Meta { return c.inner.Meta() }

func (c *controlled) AsTool(ctx context.Context) (tool.BaseTool, error) {
	inner, err := c.inner.AsTool(ctx)
	if err != nil {
		return nil, err
	}
	inv, ok := inner.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("capability %s is not invokable", c.inner.Meta().Ref)
	}
	return &controlledTool{inner: inv}, nil
}

func (c *controlled) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return runControlled(ctx, func(ctx context.Context) (string, error) {
			return capability.Invoke(ctx, c.inner, argsJSON)
		})
	}), nil
}

type controlledTool struct {
	inner tool.InvokableTool
}

func (t *controlledTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *controlledTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	return runControlled(ctx, func(ctx context.Context) (string, error) {
		return t.inner.InvokableRun(ctx, argsJSON, opts...)
	})
}

func runControlled(ctx context.Context, exec func(ctx context.Context) (string, error)) (string, error) {
	cs := controlFrom(ctx)
	if cs == nil {
		return exec(ctx)
	}
	if cs.Interrupted() {
		return "", &ErrInterrupted{}
	}
	out, err := exec(ctx)
	if err != nil {
		if cs.Interrupted() {
			return "", &ErrInterrupted{}
		}
		return out, err
	}
	if msgs := cs.drain(); len(msgs) > 0 {
		out += "\n\n[用户插话,请优先响应]\n- " + strings.Join(msgs, "\n- ")
	}
	return out, nil
}
