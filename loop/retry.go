package loop

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// RetryConfig 控制模型调用的瞬时错误重试(Ring 0)。
// 零值即启用默认策略;MaxAttempts 为 1 或负数时不重试。
type RetryConfig struct {
	// MaxAttempts 是总尝试次数(含首次),0 = 默认 3。
	MaxAttempts int `yaml:"max_attempts" json:"max_attempts"`
	// BaseDelay 是首次退避时长,按尝试次数指数增长,0 = 默认 500ms。
	BaseDelay Duration `yaml:"base_delay" json:"base_delay"`
	// MaxDelay 是单次退避上限,0 = 默认 8s。
	MaxDelay Duration `yaml:"max_delay" json:"max_delay"`
}

func (c RetryConfig) attempts() int {
	if c.MaxAttempts == 0 {
		return 3
	}
	if c.MaxAttempts < 1 {
		return 1
	}
	return c.MaxAttempts
}

func (c RetryConfig) baseDelay() time.Duration {
	if c.BaseDelay == 0 {
		return 500 * time.Millisecond
	}
	return c.BaseDelay.Std()
}

func (c RetryConfig) maxDelay() time.Duration {
	if c.MaxDelay == 0 {
		return 8 * time.Second
	}
	return c.MaxDelay.Std()
}

// RetryModel 给模型套上瞬时错误重试(指数退避)。只重试限流/瞬时
// 服务端/网络类错误;预算耗尽、参数错误等确定性失败立即返回。
// 应包在预算控制内侧:重试属于同一次逻辑调用,不重复计费预算次数。
func RetryModel(m model.ToolCallingChatModel, cfg RetryConfig) model.ToolCallingChatModel {
	if cfg.attempts() <= 1 {
		return m
	}
	return &retryModel{inner: m, cfg: cfg}
}

type retryModel struct {
	inner model.ToolCallingChatModel
	cfg   RetryConfig
}

func (r *retryModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var lastErr error
	for attempt := 0; attempt < r.cfg.attempts(); attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, r.cfg, attempt); err != nil {
				return nil, lastErr
			}
		}
		out, err := r.inner.Generate(ctx, msgs, opts...)
		if err == nil {
			return out, nil
		}
		if !Transient(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// Stream 只重试建立流之前的错误;流一旦开始传输,中途断流无法安全重放。
func (r *retryModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	var lastErr error
	for attempt := 0; attempt < r.cfg.attempts(); attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, r.cfg, attempt); err != nil {
				return nil, lastErr
			}
		}
		sr, err := r.inner.Stream(ctx, msgs, opts...)
		if err == nil {
			return sr, nil
		}
		if !Transient(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *retryModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := r.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &retryModel{inner: inner, cfg: r.cfg}, nil
}

// sleepBackoff 按尝试次数指数退避,ctx 取消时提前返回错误。
func sleepBackoff(ctx context.Context, cfg RetryConfig, attempt int) error {
	d := cfg.baseDelay() << (attempt - 1)
	if max := cfg.maxDelay(); d > max {
		d = max
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// transientMarkers 是瞬时错误的启发式特征(小写匹配),覆盖主流厂商的
// 限流与瞬时服务端错误文案。
var transientMarkers = []string{
	"429", "rate limit", "too many requests",
	"500", "502", "503", "504",
	"internal server error", "bad gateway", "service unavailable", "gateway timeout",
	"overloaded", "temporarily",
	"timeout", "timed out", "deadline exceeded",
	"connection reset", "connection refused", "broken pipe", "unexpected eof",
}

// Transient 判断错误是否值得重试。预算耗尽与 ctx 取消永不重试。
func Transient(err error) bool {
	if err == nil {
		return false
	}
	var exhausted *ErrBudgetExhausted
	if errors.As(err, &exhausted) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range transientMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
