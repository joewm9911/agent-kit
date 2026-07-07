package feishu

// TestLiveFeishuOutbound:真实飞书应用的出站链路冒烟——token 获取、
// 文本/卡片发送、卡片更新(模式1 的原语)、话题回复(reply_in_thread)。
// 入站 webhook 需要公网地址,不在此测试范围(见 dispatcher 假件测试)。
//
// 运行方式(key 只经环境变量,不落仓库与日志):
//
//	FEISHU_APP_ID=$(security find-generic-password -a agent-kit -s feishu-app-id -w) \
//	FEISHU_APP_SECRET=$(security find-generic-password -a agent-kit -s feishu-app-secret -w) \
//	FEISHU_TEST_CHAT=oc_xxx \
//	SMOKE_LIVE=1 go test ./impl/channel/feishu/ -run TestLiveFeishuOutbound -v -count=1

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/protocol/channel"
)

func TestLiveFeishuOutbound(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" {
		t.Skip("SMOKE_LIVE 未开启(真机测试需显式触发)")
	}
	appID, secret, chat := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET"), os.Getenv("FEISHU_TEST_CHAT")
	if appID == "" || secret == "" || chat == "" {
		t.Skip("需要 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_TEST_CHAT")
	}
	f, err := New("live", Config{AppID: appID, AppSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conv := channel.ConvRef{Channel: "live", Chat: chat}

	// 1. 纯文本
	textID, err := f.Send(ctx, conv, channel.Outbound{Text: "[live冒烟] 文本消息"})
	if err != nil || textID == "" {
		t.Fatalf("send text: %v id=%q", err, textID)
	}

	// 2. 卡片 + 更新(模式1:占位 → 终稿)
	cardID, err := f.Send(ctx, conv, channel.Outbound{Text: "**[live冒烟]** 处理中...", Markdown: true})
	if err != nil || cardID == "" {
		t.Fatalf("send card: %v id=%q", err, cardID)
	}
	time.Sleep(1 * time.Second) // 肉眼可辨的更新间隔
	if err := f.Update(ctx, conv, cardID, channel.Outbound{Text: "**[live冒烟]** ✅ 处理完成(卡片已更新)", Markdown: true}); err != nil {
		t.Fatalf("update card: %v", err)
	}

	// 3. 话题回复:以第 1 条消息为锚 reply_in_thread,飞书会就地起话题
	topicConv := conv
	topicConv.Thread = "live-topic" // 非空即触发话题路由;真实 thread_id 来自入站事件
	topicConv.Anchor = textID
	threadID, err := f.Send(ctx, topicConv, channel.Outbound{Text: "**[live冒烟]** 话题内回复(reply_in_thread)", Markdown: true})
	if err != nil || threadID == "" {
		t.Fatalf("thread reply: %v id=%q", err, threadID)
	}
	t.Logf("text=%s card=%s thread=%s(去群里看三条消息与卡片更新)", textID, cardID, threadID)
}
