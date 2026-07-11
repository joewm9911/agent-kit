package config

// 双装配路径(单文件 Build / 多文件 BuildApp)漂移回归:审计 2026-07 抓到
// 四处只在一条路径生效的接线(texts、HTTP 挂起、遗留键拒收、logger 穿透)。
// 本文件锁住可观察的三处;logger 穿透无外部观察面,靠 review 守。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/joewm9911/agent-kit/protocol/channel"
)

var registerDriftChannel sync.Once

type nopChannel struct{ name string }

func (c nopChannel) Name() string { return c.name }
func (c nopChannel) Start(context.Context, *http.ServeMux, channel.InboundHandler) error {
	return nil
}
func (c nopChannel) Send(context.Context, channel.ConvRef, channel.Outbound) (string, error) {
	return "", nil
}
func (c nopChannel) Update(context.Context, channel.ConvRef, string, channel.Outbound) error {
	return nil
}

func setupDriftFakes() {
	setupAppTestFakes()
	registerDriftChannel.Do(func() {
		channel.Register("nopchan", func(name string, _ map[string]any) (channel.Channel, error) {
			return nopChannel{name: name}, nil
		})
	})
}

// 单文件 Build 必须解析并接线 channel.texts(此前只有 BuildApp 有,
// 单文件路径静默丢弃文案覆盖)。未知键 fail fast 是可观察面。
func TestBuildWiresChannelTexts(t *testing.T) {
	setupDriftFakes()
	path := writeTree(t, map[string]string{"app.yaml": `
model: {provider: marker, config: {resp: hi}}
agents:
  - name: helper
serving: {addr: "127.0.0.1:0"}
channels:
  - name: c1
    type: nopchan
    agent: helper
    texts: {no_such_text_key: "x"}
`})
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), cfg, BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "no_such_text_key") {
		t.Fatalf("single-file Build must parse channel texts (unknown key fail-fast), got %v", err)
	}
}

// 多文件 BuildApp 必须拒收 app 级遗留键(此前只有单文件 Build 拒,
// 多文件路径静默接受 max_steps)。
func TestBuildAppRejectsLegacyKeys(t *testing.T) {
	setupDriftFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: hi}}
loop: {max_steps: 5}
agents: [agents/helper.yaml]
`,
		"agents/helper.yaml": "description: 测试\n",
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildApp(context.Background(), spec, BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "max_rounds") {
		t.Fatalf("BuildApp must reject legacy max_steps with migration hint, got %v", err)
	}
}

// 严格 YAML:拼错的配置键必须装配期报错,而不是静默忽略。
func TestStrictYAMLRejectsUnknownKey(t *testing.T) {
	setupDriftFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: hi}}
agents: [agents/helper.yaml]
`,
		"agents/helper.yaml": "descripton: 拼错的键\n", // description 少了 i
	})
	_, err := LoadApp(appPath)
	if err == nil || !strings.Contains(err.Error(), "descripton") {
		t.Fatalf("strict yaml must name the unknown field, got %v", err)
	}
}
