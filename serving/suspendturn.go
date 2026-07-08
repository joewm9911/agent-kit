package serving

// 挂起轮次的共享编排(机制层):resumePending 把"会话的下一条输入是
// 答案"变成事实,beginSuspendTurn/finish 包住一轮的 journal 装配与挂起
// 收口。Dispatcher(IM)与 Server(HTTP /messages)共用这一份,只在
// 传输策略上分岔:问句怎么送达(IM 发进会话,HTTP 落响应体)、waiting
// 怎么呈现(占位卡收口,{status: waiting})。中断/插话同理——IM 的
// 「停止/插话:」文本指令与 HTTP 的 /control 端点是同一
// Agent.Interrupt/Steer 机制的两个传输入口。

import (
	"context"
	"errors"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// resumePending 查询并认领会话的挂起轮次:命中则把本条输入记为答案,
// 返回待重放的轮次记录(以原输入 + 原轮次 ID 重放)。取出与记答案必须
// 成对——只取不记会永久丢失该轮;记答案失败时回滚挂起记录,调用方
// 报错即可,用户重发一遍答案还接得上。
func resumePending(ctx context.Context, kv store.KV, sessionKey, input string) (suspend.PendingTurn, bool, error) {
	rec, ok, err := suspend.TakePendingTurn(ctx, kv, sessionKey)
	if err != nil || !ok {
		return suspend.PendingTurn{}, false, err
	}
	if err := suspend.AnswerPending(ctx, kv, rec.WaitingID, input); err != nil {
		_ = suspend.SavePendingTurn(ctx, kv, sessionKey, rec) // 认领回滚,挂起不丢
		return suspend.PendingTurn{}, false, err
	}
	return rec, true, nil
}

// suspendTurn 是一轮挂起模式执行的收口句柄。
type suspendTurn struct {
	kv      store.KV
	journal *suspend.Journal
}

// beginSuspendTurn 装配一轮挂起模式的执行环境:journal 与可挂起交互
// 通道注入 ctx。turnID 空 = 新轮次;恢复的轮次必须沿用首跑 ID(交互/
// 效果日志按轮次分键,换 ID 即重放失忆)。notify 是问句投递回调,
// 没有独立投递面的传输(HTTP:问句即响应体)传 nil。
func beginSuspendTurn(ctx context.Context, kv store.KV, turnID string, notify suspend.Notify) (context.Context, *suspendTurn) {
	if turnID == "" {
		turnID = suspend.NewTurnID()
	}
	if notify == nil {
		notify = func(context.Context, string) error { return nil }
	}
	j := suspend.NewJournal(kv, turnID)
	ctx = suspend.WithJournal(ctx, j)
	ctx = runctx.WithInteractor(ctx, suspend.Interactor(j, notify))
	return ctx, &suspendTurn{kv: kv, journal: j}
}

// finish 收口一轮:runErr 是挂起信号 → 持久化待答轮次并返回问句
// (持久化失败按错误收口,不能让用户以为"回复后继续"而实际接不上);
// 善终 → 清理该轮日志;其余错误原样透传。
func (t *suspendTurn) finish(ctx context.Context, sessionKey, turnInput string, runErr error) (question string, suspended bool, err error) {
	if runErr == nil {
		t.journal.CompleteTurn(ctx)
		return "", false, nil
	}
	var s *suspend.ErrSuspended
	if !errors.As(runErr, &s) {
		return "", false, runErr
	}
	if err := suspend.SavePendingTurn(ctx, t.kv, sessionKey, suspend.PendingTurn{
		TurnID: t.journal.TurnID(), Input: turnInput, WaitingID: s.InteractionID,
	}); err != nil {
		return "", false, err
	}
	return s.Question, true, nil
}
