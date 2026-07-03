package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/cloverzhang/agent-kit/runctx"
)

// Event 是轨迹里的一条记录。改提示词、换工具描述后跑回归评测,
// 以及"某次坏回答对应哪个版本"的回溯,都以这份数据为底座。
type Event struct {
	Time      time.Time `json:"ts"`
	Agent     string    `json:"agent,omitempty"`
	Session   string    `json:"session,omitempty"`
	Component string    `json:"component"`
	Name      string    `json:"name,omitempty"`
	Phase     string    `json:"phase"` // start | end | error
	CostMS    int64     `json:"cost_ms,omitempty"`
	Tokens    int       `json:"tokens,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Trajectory 返回把执行轨迹写入 JSONL 文件的 callbacks.Handler。
// 每个组件(模型、工具、图节点)的开始/结束/耗时/用量一条记录。
func Trajectory(path string) (callbacks.Handler, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("trajectory: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("trajectory: open file: %w", err)
	}
	w := &writer{f: f}

	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			w.emit(ctx, info, Event{Phase: "start"})
			return context.WithValue(ctx, trajKey{}, time.Now())
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			ev := Event{Phase: "end", CostMS: sinceMS(ctx)}
			if mo := einomodel.ConvCallbackOutput(output); mo != nil && mo.TokenUsage != nil {
				ev.Tokens = mo.TokenUsage.TotalTokens
			}
			w.emit(ctx, info, ev)
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			w.emit(ctx, info, Event{Phase: "error", CostMS: sinceMS(ctx), Error: err.Error()})
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			output.Close() // 打点不消费流,必须关闭副本防泄漏
			w.emit(ctx, info, Event{Phase: "end", CostMS: sinceMS(ctx)})
			return ctx
		}).
		Build(), nil
}

type trajKey struct{}

func sinceMS(ctx context.Context) int64 {
	if t, ok := ctx.Value(trajKey{}).(time.Time); ok {
		return time.Since(t).Milliseconds()
	}
	return 0
}

type writer struct {
	mu sync.Mutex
	f  *os.File
}

func (w *writer) emit(ctx context.Context, info *callbacks.RunInfo, ev Event) {
	ev.Time = time.Now()
	ev.Agent = runctx.Agent(ctx)
	ev.Session = runctx.Session(ctx)
	if info != nil {
		ev.Component = string(info.Component)
		ev.Name = info.Name
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.f.Write(append(b, '\n'))
}
