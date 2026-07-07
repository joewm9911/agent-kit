// Package channel 定义 IM 接入层(Ring 1):飞书、钉钉、Slack 等各写
// 一个适配器,agent 侧零改动。Channel 负责收发消息的传输细节,
// Dispatcher 负责会话映射、同会话串行、幂等去重,以及把 IM 对话桥接为
// HITL 交互通道(ask_user 的答案、审批的批复都来自 IM 里的下一条消息)。
package channel

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// ConvRef 定位一个 IM 会话(以及本次入站的回复路由信息)。
type ConvRef struct {
	Channel string // channel 实例名
	Chat    string // 群/单聊 ID
	User    string // 发言用户 ID
	// Thread 是话题/子讨论 ID(飞书话题的 thread_id 等):非空表示消息
	// 来自话题内——回复应发回同一话题,会话映射按话题细分(见
	// Dispatcher.sessionKey)。无话题概念的通道保持空。
	Thread string
	// Anchor 是触发本轮的入站消息 ID:话题内回复需要以话题中某条消息
	// 为锚(飞书走 reply 接口回话题)。无话题语义时可为空。
	Anchor string
}

// Inbound 是收到的一条用户消息。
type Inbound struct {
	Conv    ConvRef
	Text    string
	EventID string // 平台事件 ID,幂等去重用
}

// Outbound 是要发出的一条消息。
type Outbound struct {
	Text     string
	Markdown bool // 富文本(卡片)渲染
}

// InboundHandler 由 Dispatcher 提供,Channel 收到消息后调用。
type InboundHandler func(ctx context.Context, in Inbound)

// Channel 是 IM 适配器的最小契约。
type Channel interface {
	Name() string
	// Start 注册 webhook 路由(或建立长连接),收到用户消息时回调 h。
	Start(ctx context.Context, mux *http.ServeMux, h InboundHandler) error
	// Send 发送消息,返回消息 ID(供 Update 做流式刷新)。
	Send(ctx context.Context, conv ConvRef, msg Outbound) (msgID string, err error)
	// Update 更新已发送的消息(卡片伪流式);不支持的通道返回 ErrUpdateUnsupported。
	Update(ctx context.Context, conv ConvRef, msgID string, msg Outbound) error
}

// ErrUpdateUnsupported 表示该通道不支持消息更新,调用方应退化为整段回复。
var ErrUpdateUnsupported = fmt.Errorf("channel: message update unsupported")

// Factory 按配置构造 Channel。
type Factory func(name string, conf map[string]any) (Channel, error)

var (
	facMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册 channel 类型(feishu/dingtalk/slack/自定义)。
func Register(typ string, f Factory) {
	facMu.Lock()
	defer facMu.Unlock()
	if _, ok := factories[typ]; ok {
		panic(fmt.Sprintf("channel: type %q already registered", typ))
	}
	factories[typ] = f
}

// New 按类型实例化 Channel。
func New(typ, name string, conf map[string]any) (Channel, error) {
	facMu.RLock()
	f, ok := factories[typ]
	facMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("channel: unknown type %q", typ)
	}
	return f(name, conf)
}
