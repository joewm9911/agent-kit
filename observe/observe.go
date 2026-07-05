// Package observe 基于 eino callbacks 提供开箱即用的可观测性:
// 每个组件(模型、工具、图节点)的开始/结束/耗时/错误统一打点。
//
// 本包只提供纯构造(Handler/Trajectory 返回 callbacks.Handler),不持有
// 任何进程级状态;"装没装过"的账本由装配层(config)持有——eino 全局
// 回调是进程级切面,幂等责任属于装配进程的一方,不属于库。接入
// Langfuse / OTel 等平台时,参照 Handler 用对应 SDK 实现同样的五个时机。
package observe

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

type ctxKey struct{}

// Handler 返回一个把组件执行日志写入 slog 的 callbacks.Handler。
func Handler(logger *slog.Logger) callbacks.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			logger.Info("start",
				slog.String("component", string(info.Component)),
				slog.String("name", info.Name),
			)
			return context.WithValue(ctx, ctxKey{}, time.Now())
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			logger.Info("end",
				slog.String("component", string(info.Component)),
				slog.String("name", info.Name),
				slog.Duration("cost", cost(ctx)),
			)
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			logger.Error("error",
				slog.String("component", string(info.Component)),
				slog.String("name", info.Name),
				slog.Duration("cost", cost(ctx)),
				slog.String("err", err.Error()),
			)
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			output.Close() // 打点不消费流内容,必须关闭副本防泄漏
			logger.Info("end(stream)",
				slog.String("component", string(info.Component)),
				slog.String("name", info.Name),
				slog.Duration("cost", cost(ctx)),
			)
			return ctx
		}).
		Build()
}

func cost(ctx context.Context) time.Duration {
	if t, ok := ctx.Value(ctxKey{}).(time.Time); ok {
		return time.Since(t)
	}
	return 0
}
