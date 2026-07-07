// Package feishu 是飞书(Lark)的 Channel 适配器:
//   - webhook 事件订阅(url 验证、encrypt_key 解密、verification_token 校验);
//   - 文本/富文本卡片回复,卡片支持更新(伪流式);
//   - tenant_access_token 自动获取与缓存。
//
// 不依赖官方 SDK,直接走 OpenAPI,便于自建部署与审计。
package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/channel"
)

func init() {
	channel.Register("feishu", func(name string, conf map[string]any) (channel.Channel, error) {
		var cfg Config
		if err := decode.Config(conf, &cfg); err != nil {
			return nil, err
		}
		return New(name, cfg)
	})
}

// Config 是飞书应用配置。
type Config struct {
	AppID             string `json:"app_id"`
	AppSecret         string `json:"app_secret"`
	VerificationToken string `json:"verification_token"`
	EncryptKey        string `json:"encrypt_key"` // 空 = 明文事件
	BaseURL           string `json:"base_url"`    // 默认 https://open.feishu.cn
	Path              string `json:"path"`        // webhook 路径,默认 /webhook/feishu/<name>
	// TriggerP2P / TriggerGroup:all | mention | none,默认 p2p=all,group=mention。
	TriggerP2P   string `json:"trigger_p2p"`
	TriggerGroup string `json:"trigger_group"`
}

// Feishu 实现 channel.Channel。
type Feishu struct {
	name string
	cfg  Config
	hc   *http.Client

	tokMu     sync.Mutex
	token     string
	tokExpire time.Time
}

// New 创建飞书通道。
func New(name string, cfg Config) (*Feishu, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("feishu %q: app_id and app_secret are required", name)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://open.feishu.cn"
	}
	if cfg.Path == "" {
		cfg.Path = "/webhook/feishu/" + name
	}
	if cfg.TriggerP2P == "" {
		cfg.TriggerP2P = "all"
	}
	if cfg.TriggerGroup == "" {
		cfg.TriggerGroup = "mention"
	}
	return &Feishu{name: name, cfg: cfg, hc: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (f *Feishu) Name() string { return f.name }

// ---- 接收:webhook ----

type eventBody struct {
	Encrypt string `json:"encrypt"`
	// 明文/解密后字段
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Schema    string `json:"schema"`
	Header    struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Token     string `json:"token"`
	} `json:"header"`
	Event struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			MessageID   string            `json:"message_id"`
			ChatID      string            `json:"chat_id"`
			ChatType    string            `json:"chat_type"` // p2p | group
			ThreadID    string            `json:"thread_id"` // 话题消息才有
			MessageType string            `json:"message_type"`
			Content     string            `json:"content"`
			Mentions    []json.RawMessage `json:"mentions"`
		} `json:"message"`
	} `json:"event"`
}

// Start 注册 webhook 路由。飞书要求秒级 ACK:处理逻辑全部异步。
func (f *Feishu) Start(_ context.Context, mux *http.ServeMux, h channel.InboundHandler) error {
	mux.HandleFunc("POST "+f.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		body, err := f.decode(raw)
		if err != nil {
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}

		// URL 验证握手
		if body.Type == "url_verification" {
			if f.cfg.VerificationToken != "" && body.Token != f.cfg.VerificationToken {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": body.Challenge})
			return
		}

		if f.cfg.VerificationToken != "" && body.Header.Token != f.cfg.VerificationToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK) // 先 ACK,防平台重试

		if body.Header.EventType != "im.message.receive_v1" || body.Event.Message.MessageType != "text" {
			return
		}
		if !f.triggered(body) {
			return
		}
		var content struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(body.Event.Message.Content), &content)
		text := cleanMentions(content.Text)
		if text == "" {
			return
		}
		conv := channel.ConvRef{
			Channel: f.name,
			Chat:    body.Event.Message.ChatID,
			User:    body.Event.Sender.SenderID.OpenID,
		}
		if body.Event.Message.ThreadID != "" { // 话题消息:回复回话题,会话按话题细分
			conv.Thread = body.Event.Message.ThreadID
			conv.Anchor = body.Event.Message.MessageID
		}
		go h(context.Background(), channel.Inbound{
			Conv:    conv,
			Text:    text,
			EventID: body.Header.EventID,
		})
	})
	return nil
}

func (f *Feishu) triggered(body *eventBody) bool {
	mode := f.cfg.TriggerP2P
	if body.Event.Message.ChatType == "group" {
		mode = f.cfg.TriggerGroup
	}
	switch mode {
	case "all":
		return true
	case "mention":
		return body.Event.Message.ChatType == "p2p" || len(body.Event.Message.Mentions) > 0
	default:
		return false
	}
}

var mentionPattern = regexp.MustCompile(`@_user_\d+\s*`)

func cleanMentions(s string) string {
	return strings.TrimSpace(mentionPattern.ReplaceAllString(s, ""))
}

// decode 解出事件体:配置了 encrypt_key 时按飞书规范 AES-CBC 解密
// (key=sha256(encrypt_key),密文前 16 字节为 IV)。
func (f *Feishu) decode(raw []byte) (*eventBody, error) {
	var body eventBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Encrypt == "" {
		return &body, nil
	}
	if f.cfg.EncryptKey == "" {
		return nil, fmt.Errorf("received encrypted event but encrypt_key not configured")
	}
	data, err := base64.StdEncoding.DecodeString(body.Encrypt)
	if err != nil {
		return nil, err
	}
	if len(data) < aes.BlockSize {
		return nil, fmt.Errorf("cipher too short")
	}
	key := sha256.Sum256([]byte(f.cfg.EncryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	iv, ct := data[:aes.BlockSize], data[aes.BlockSize:]
	if len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bad cipher length")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(ct, ct)
	// PKCS7 去填充
	if n := int(ct[len(ct)-1]); n > 0 && n <= aes.BlockSize && n <= len(ct) {
		ct = ct[:len(ct)-n]
	}
	var out eventBody
	if err := json.Unmarshal(ct, &out); err != nil {
		return nil, fmt.Errorf("decrypt: bad plaintext: %w", err)
	}
	return &out, nil
}

// ---- 发送:OpenAPI ----

// Send 发送消息:Markdown=true 用可更新的交互卡片,否则纯文本。
// 话题消息(conv.Thread 非空)以入站消息为锚走 reply 接口、
// reply_in_thread 落回同一话题,不散到主聊天流。
func (f *Feishu) Send(ctx context.Context, conv channel.ConvRef, msg channel.Outbound) (string, error) {
	msgType, content := encode(msg)
	var resp struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if conv.Thread != "" && conv.Anchor != "" {
		err := f.call(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+conv.Anchor+"/reply",
			map[string]any{"msg_type": msgType, "content": content, "reply_in_thread": true}, &resp)
		return resp.Data.MessageID, err
	}
	payload := map[string]string{"receive_id": conv.Chat, "msg_type": msgType, "content": content}
	err := f.call(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=chat_id", payload, &resp)
	return resp.Data.MessageID, err
}

// Update 更新交互卡片内容(伪流式的刷新通道)。
func (f *Feishu) Update(ctx context.Context, _ channel.ConvRef, msgID string, msg channel.Outbound) error {
	if !msg.Markdown {
		return channel.ErrUpdateUnsupported // 纯文本消息不可更新
	}
	_, content := encode(channel.Outbound{Text: msg.Text, Markdown: true})
	return f.call(ctx, http.MethodPatch, "/open-apis/im/v1/messages/"+msgID,
		map[string]string{"content": content}, nil)
}

func encode(msg channel.Outbound) (msgType, content string) {
	if !msg.Markdown {
		b, _ := json.Marshal(map[string]string{"text": msg.Text})
		return "text", string(b)
	}
	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true, "update_multi": true},
		"elements": []any{map[string]any{"tag": "markdown", "content": msg.Text}},
	}
	b, _ := json.Marshal(card)
	return "interactive", string(b)
}

func (f *Feishu) call(ctx context.Context, method, path string, payload, out any) error {
	tok, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, f.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("feishu: bad response: %s", data)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("feishu API error %d: %s", envelope.Code, envelope.Msg)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (f *Feishu) accessToken(ctx context.Context) (string, error) {
	f.tokMu.Lock()
	defer f.tokMu.Unlock()
	if f.token != "" && time.Now().Before(f.tokExpire) {
		return f.token, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": f.cfg.AppID, "app_secret": f.cfg.AppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.cfg.BaseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu get token: %d %s", out.Code, out.Msg)
	}
	f.token = out.Token
	f.tokExpire = time.Now().Add(time.Duration(out.Expire-60) * time.Second)
	return f.token, nil
}
