// longconn.go:飞书长连接事件接收(mode: long_conn)。
//
// 机器人主动向飞书建立 WebSocket,事件经连接推送——不需要公网地址,
// 本地/内网即可收消息。线上协议是飞书私有 protobuf 帧,自实现不可
// 审计,此处引官方 SDK 只做传输;事件落地后立刻转成 msgEvent,与
// webhook 模式共用 deliver 归一化路径,SDK 不渗透到包外。
//
// 开放平台侧要求:应用的事件订阅方式须选择"使用长连接接收事件",
// 并订阅 im.message.receive_v1。
package feishu

import (
	"context"
	"log/slog"
	"os"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/joewm9911/agent-kit/protocol/channel"
)

// startLongConn 建立长连接并把消息事件送入共用入站路径。
// SDK 内置断线重连;Start 返回错误只发生在建连参数级失败,记日志由
// 重连兜底,不拉垮进程(通道故障不该杀死整个 app)。
func (f *Feishu) startLongConn(ctx context.Context, h channel.InboundHandler) error {
	d := dispatcher.NewEventDispatcher("", ""). // 长连接不校验 token/加密(传输层已鉴权)
							OnP2MessageReceiveV1(func(evCtx context.Context, e *larkim.P2MessageReceiveV1) error {
			ev, ok := normalizeWS(e)
			if !ok {
				return nil
			}
			// 秒级 ACK 后异步处理:剥离 SDK 回调 ctx 的取消,保留其值——与
			// webhook 路径一致,让 trace baggage(若 SDK 透传)穿到下游。
			go f.deliver(context.WithoutCancel(evCtx), h, ev)
			return nil
		})
	logLevel := larkcore.LogLevelInfo
	if os.Getenv("FEISHU_WS_DEBUG") != "" { // 帧级排障开关(仅日志粒度)
		logLevel = larkcore.LogLevelDebug
	}
	cli := ws.NewClient(f.cfg.AppID, f.cfg.AppSecret,
		ws.WithEventHandler(d),
		ws.WithDomain(f.cfg.BaseURL),
		ws.WithAutoReconnect(true),
		ws.WithLogLevel(logLevel),
	)
	go func() {
		if err := cli.Start(ctx); err != nil && ctx.Err() == nil {
			slog.Error("feishu long_conn exited", slog.String("channel", f.name), slog.String("err", err.Error()))
		}
	}()
	return nil
}

// normalizeWS 把 SDK 事件转成归一化 msgEvent(SDK 类型不出此文件)。
func normalizeWS(e *larkim.P2MessageReceiveV1) (msgEvent, bool) {
	if e == nil || e.Event == nil || e.Event.Message == nil {
		return msgEvent{}, false
	}
	m := e.Event.Message
	ev := msgEvent{
		msgID:    strDeref(m.MessageId),
		chatID:   strDeref(m.ChatId),
		chatType: strDeref(m.ChatType),
		threadID: strDeref(m.ThreadId),
		msgType:  strDeref(m.MessageType),
		content:  strDeref(m.Content),
		mentions: len(m.Mentions),
	}
	if e.EventV2Base != nil && e.EventV2Base.Header != nil {
		ev.eventID = e.EventV2Base.Header.EventID
	}
	if s := e.Event.Sender; s != nil && s.SenderId != nil {
		ev.openID = strDeref(s.SenderId.OpenId)
	}
	return ev, true
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
