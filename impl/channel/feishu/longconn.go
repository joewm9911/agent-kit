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
	"time"

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
			// WithoutCancel 对 nil 会 panic,SDK 理论上非 nil,仍兜底防炸。
			base := evCtx
			if base == nil {
				base = context.Background()
			}
			go f.deliver(context.WithoutCancel(base), h, ev)
			return nil
		})
	logLevel := larkcore.LogLevelInfo
	if os.Getenv("FEISHU_WS_DEBUG") != "" { // 帧级排障开关(仅日志粒度)
		logLevel = larkcore.LogLevelDebug
	}
	newClient := func() *ws.Client {
		return ws.NewClient(f.cfg.AppID, f.cfg.AppSecret,
			ws.WithEventHandler(d),
			ws.WithDomain(f.cfg.BaseURL),
			ws.WithAutoReconnect(true),
			ws.WithLogLevel(logLevel),
		)
	}
	// 监督循环:SDK 的 AutoReconnect 只兜它自己认出的断线;实测网络闪断
	// (代理掐连接)后 Start 会直接返回——返回 nil 时旧代码连日志都不打,
	// goroutine 静默退场,机器人从此收不到任何消息且无迹可查。这里无论
	// 返回什么都记日志、退避后重建客户端再连;退避封顶 2 分钟,恢复即清零。
	go func() {
		backoff := time.Second
		for {
			began := time.Now()
			err := newClient().Start(ctx)
			if ctx.Err() != nil {
				return
			}
			if time.Since(began) > 5*time.Minute {
				backoff = time.Second // 连得住说明网络已恢复,退避从头算
			}
			if err != nil {
				slog.Error("feishu long_conn exited; will reconnect",
					slog.String("channel", f.name), slog.Duration("backoff", backoff), slog.String("err", err.Error()))
			} else {
				slog.Error("feishu long_conn exited without error; will reconnect",
					slog.String("channel", f.name), slog.Duration("backoff", backoff))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
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
