package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/protocol/channel"
)

// fakeOpenAPI 模拟飞书 OpenAPI:token、发消息、话题回复三个端点,
// 记录每次调用的路径与请求体。
type fakeOpenAPI struct {
	mu    sync.Mutex
	calls []struct {
		Path string
		Body map[string]any
	}
}

func (s *fakeOpenAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code":0,"tenant_access_token":"t","expire":7200}`)
	})
	record := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.calls = append(s.calls, struct {
			Path string
			Body map[string]any
		}{r.URL.Path, body})
		s.mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"om_new"}}`)
	}
	mux.HandleFunc("/open-apis/im/v1/messages", record)
	mux.HandleFunc("/open-apis/im/v1/messages/", record) // reply / patch
	return mux
}

func (s *fakeOpenAPI) last(t *testing.T) (string, map[string]any) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("no OpenAPI calls recorded")
	}
	c := s.calls[len(s.calls)-1]
	return c.Path, c.Body
}

func newTestFeishu(t *testing.T) (*Feishu, *fakeOpenAPI) {
	t.Helper()
	api := &fakeOpenAPI{}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	f, err := New("f1", Config{AppID: "a", AppSecret: "s", BaseURL: srv.URL, Mode: "webhook"})
	if err != nil {
		t.Fatal(err)
	}
	return f, api
}

// TestSendRoutesThreadReply:话题消息以锚点走 reply 接口且 reply_in_thread;
// 普通消息保持 chat_id 直发。
func TestSendRoutesThreadReply(t *testing.T) {
	f, api := newTestFeishu(t)
	ctx := context.Background()

	topic := channel.ConvRef{Channel: "f1", Chat: "oc_1", Thread: "omt_9", Anchor: "om_5"}
	if _, err := f.Send(ctx, topic, channel.Outbound{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	path, body := api.last(t)
	if path != "/open-apis/im/v1/messages/om_5/reply" {
		t.Fatalf("topic send path = %q", path)
	}
	if body["reply_in_thread"] != true {
		t.Fatalf("reply_in_thread missing: %+v", body)
	}

	plain := channel.ConvRef{Channel: "f1", Chat: "oc_1"}
	if _, err := f.Send(ctx, plain, channel.Outbound{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if path, _ = api.last(t); path != "/open-apis/im/v1/messages" {
		t.Fatalf("plain send path = %q", path)
	}
}

// TestWebhookParsesThread:webhook 事件里的 thread_id/message_id 进入
// ConvRef 的 Thread/Anchor;非话题消息两者为空。
func TestWebhookParsesThread(t *testing.T) {
	f, _ := newTestFeishu(t)
	mux := http.NewServeMux()
	var (
		mu  sync.Mutex
		got []channel.Inbound
	)
	if err := f.Start(context.Background(), mux, func(_ context.Context, in channel.Inbound) {
		mu.Lock()
		got = append(got, in)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	event := func(threadID string) string {
		return fmt.Sprintf(`{"schema":"2.0","header":{"event_id":"e-%s","event_type":"im.message.receive_v1"},
		"event":{"sender":{"sender_id":{"open_id":"u1"}},
		"message":{"message_id":"om_5","chat_id":"oc_1","chat_type":"p2p","thread_id":%q,
		"message_type":"text","content":"{\"text\":\"你好\"}"}}}`, threadID, threadID)
	}
	for _, tid := range []string{"omt_9", ""} {
		resp, err := http.Post(srv.URL+f.cfg.Path, "application/json", strings.NewReader(event(tid)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("inbound = %d, want 2", len(got))
	}
	topic, plain := got[0], got[1]
	if topic.Conv.Thread != "omt_9" || topic.Conv.Anchor != "om_5" {
		t.Fatalf("topic conv = %+v", topic.Conv)
	}
	if plain.Conv.Thread != "" || plain.Conv.Anchor != "" {
		t.Fatalf("plain conv should have no thread: %+v", plain.Conv)
	}
}

// TestNativePassthrough:Outbound.Native 原样透传(Send 与 Update 均是),
// 不经 encode 的默认卡片包装。
func TestNativePassthrough(t *testing.T) {
	f, api := newTestFeishu(t)
	ctx := context.Background()
	conv := channel.ConvRef{Channel: "f1", Chat: "oc_1"}
	native := map[string]any{
		"config":   map[string]any{"update_multi": true},
		"header":   map[string]any{"template": "blue"},
		"elements": []any{map[string]any{"tag": "markdown", "content": "自定义"}},
	}

	if _, err := f.Send(ctx, conv, channel.Outbound{Native: native}); err != nil {
		t.Fatal(err)
	}
	_, body := api.last(t)
	var card map[string]any
	if err := json.Unmarshal([]byte(body["content"].(string)), &card); err != nil {
		t.Fatal(err)
	}
	if body["msg_type"] != "interactive" || card["header"].(map[string]any)["template"] != "blue" {
		t.Fatalf("native not passed through: %+v", body)
	}

	if err := f.Update(ctx, conv, "om_1", channel.Outbound{Native: native}); err != nil {
		t.Fatal(err)
	}
	path, ubody := api.last(t)
	if path != "/open-apis/im/v1/messages/om_1" {
		t.Fatalf("update path = %q", path)
	}
	if err := json.Unmarshal([]byte(ubody["content"].(string)), &card); err != nil || card["header"] == nil {
		t.Fatalf("native update not passed through: %v %+v", err, ubody)
	}
}
