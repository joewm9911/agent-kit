// decorate.go:出站装饰与进度订阅的第三方扩展面
// (设计见 docs/channel-card-design.md §2.4/§3)。
//
// 框架给事实(Outbound 语义字段、ProgressEvent 事件流),呈现策略
// 100% 归第三方:装饰器改文案、构造 Native 整卡、或置 Skip 否决发送;
// 进度订阅者拿原始事件自行控制节奏与形态。嵌入方直接塞函数值到
// Binding;配置方经按名注册表(init 自注册、运行期只读、装配期查名
// fail fast,与 model/source 同一惯例)。
package serving

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
)

// Decorator 应用于每条出站消息(占位/过程更新/终稿/问句/错误/通知),
// 在 channel.Send/Update 之前调用。读语义事实(Kind/Text/Progress/
// Meta),三种产出:改写语义字段(适配器默认渲染)、构造 Native
// (适配器原样透传)、置 Skip(本步不发送)。
type Decorator func(ctx context.Context, conv channel.ConvRef, out channel.Outbound) channel.Outbound

// ProgressHandler 是绑定级进度订阅者:装了它,内置的卡片过程更新让位,
// IM 呈现完全由第三方接管。投递经异步有界队列,慢/挂不影响执行主流程。
type ProgressHandler func(ctx context.Context, conv channel.ConvRef, ev runctx.ProgressEvent)

var (
	extMu       sync.RWMutex
	decorators  = map[string]Decorator{}
	progressers = map[string]ProgressHandler{}
)

// RegisterDecorator 按名注册装饰器(init 期调用;重名 panic,装配期暴露)。
func RegisterDecorator(name string, d Decorator) {
	extMu.Lock()
	defer extMu.Unlock()
	if _, ok := decorators[name]; ok {
		panic(fmt.Sprintf("serving: decorator %q already registered", name))
	}
	decorators[name] = d
}

// LookupDecorator 按名解析装饰器(装配层用,查无报错 fail fast)。
func LookupDecorator(name string) (Decorator, error) {
	extMu.RLock()
	defer extMu.RUnlock()
	d, ok := decorators[name]
	if !ok {
		return nil, fmt.Errorf("serving: unknown decorator %q (call RegisterDecorator first)", name)
	}
	return d, nil
}

// RegisterProgressHandler 按名注册进度订阅者。
func RegisterProgressHandler(name string, h ProgressHandler) {
	extMu.Lock()
	defer extMu.Unlock()
	if _, ok := progressers[name]; ok {
		panic(fmt.Sprintf("serving: progress handler %q already registered", name))
	}
	progressers[name] = h
}

// LookupProgressHandler 按名解析进度订阅者。
func LookupProgressHandler(name string) (ProgressHandler, error) {
	extMu.RLock()
	defer extMu.RUnlock()
	h, ok := progressers[name]
	if !ok {
		return nil, fmt.Errorf("serving: unknown progress handler %q (call RegisterProgressHandler first)", name)
	}
	return h, nil
}

// decorate 应用绑定的装饰器:装饰器 panic 时用未装饰的原始消息兜底
// (装饰失败不能吞消息——诚实优先于好看)。
func decorate(ctx context.Context, b Binding, conv channel.ConvRef, out channel.Outbound) (decorated channel.Outbound) {
	if b.Decorator == nil {
		return out
	}
	decorated = out
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("decorator panicked, sending undecorated", slog.Any("panic", r), slog.String("kind", out.Kind))
			decorated = out
		}
	}()
	return b.Decorator(ctx, conv, out)
}
