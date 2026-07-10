package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/source"
)

var registerAppTestFakes sync.Once

// markerModel 固定返回构造时的标记文本,用于验证 override 链选中了哪个模型。
type markerModel struct{ resp string }

func (m *markerModel) Generate(context.Context, []*schema.Message, ...einomodel.Option) (*schema.Message, error) {
	return schema.AssistantMessage(m.resp, nil), nil
}
func (m *markerModel) Stream(ctx context.Context, in []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, _ := m.Generate(ctx, in, opts...)
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}
func (m *markerModel) WithTools([]*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return m, nil
}

var syncCount atomic.Int32

func setupAppTestFakes() {
	setupTestSource() // nstest(见 namespace_test.go)
	registerAppTestFakes.Do(func() {
		// marker 模型:config.resp 即固定回复
		model.Register("marker", func(_ context.Context, conf map[string]any) (einomodel.ToolCallingChatModel, error) {
			resp, _ := conf["resp"].(string)
			return &markerModel{resp: resp}, nil
		})
		// countsrc:统计 Sync 次数,验证源连接缓存
		source.Register("countsrc", func(_ context.Context, name string, _ map[string]any) (source.Source, error) {
			return countingSource{name: name}, nil
		})
	})
}

type countingSource struct{ name string }

func (s countingSource) Name() string { return s.name }
func (s countingSource) Sync(context.Context) ([]capability.Capability, error) {
	syncCount.Add(1)
	return []capability.Capability{capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: s.name, Name: "ping"},
	}, func(_ context.Context, in string) (string, error) { return "pong", nil })}, nil
}

// writeTree 在临时目录写一组配置文件,返回 app.yaml 路径。
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "app.yaml")
}

func TestLoadAppConventions(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: app-model}}
agents: [agents/helper.yaml]
`,
		"agents/helper.yaml": `
description: 测试
namespaces: [../namespaces/ops.yaml]
`,
		"namespaces/ops.yaml": `
sources:
  - {name: svc, type: nstest}
skills:
  - name: lookup
    steps:
      - {name: s, use: "tools/svc/search"}
`,
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	// 文件名即名字
	if spec.Agents[0].Name != "helper" {
		t.Fatalf("agent name = %q", spec.Agents[0].Name)
	}
	if spec.Agents[0].Mounts[0].Name != "ops" {
		t.Fatalf("namespace name = %q", spec.Agents[0].Mounts[0].Name)
	}

	app, err := BuildApp(context.Background(), spec, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if app.Agents["helper"] == nil {
		t.Fatal("agent helper not built")
	}
	// 自动挂载:关联 namespace 的导出 skill 进了 agent 挂载目录
	if _, err := app.AgentMounts["helper"].Get("cap://skill/ops/lookup"); err != nil {
		t.Fatalf("auto-mounted skill missing: %v", err)
	}
}

func TestLoadAppNameMismatch(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml":           "agents: [agents/helper.yaml]\n",
		"agents/helper.yaml": "name: other\n",
	})
	_, err := LoadApp(appPath)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expect name mismatch error, got %v", err)
	}
}

// TestOverrideChainModel 验证 model 的三级降级(能力不可自指 model):
// agent 给 namespace 的 per-mount 指定 > agent 自己 > app;namespace/component
// 内不可写 model。agent a 有自己的 model,并给 over 这个 mount 显式指定
// mount-model(压过其全部 component);plain mount 不指定(component 回落
// agent 自己)。agent b 无自己的 model(回落 app)。
func TestOverrideChainModel(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: app-model}}
agents: [agents/a.yaml, agents/b.yaml]
`,
		"agents/a.yaml": `
model: {provider: marker, config: {resp: agent-model}}
namespaces:
  - path: ../namespaces/over.yaml
    model: {provider: marker, config: {resp: mount-model}}
  - ../namespaces/plain.yaml
`,
		// agent b 不声明自己的 model → component 回落 app-model
		"agents/b.yaml": `
namespaces: [../namespaces/plain.yaml]
`,
		// per-mount 覆盖压过全部 component(component 内不可写 model)
		"namespaces/over.yaml": `
components:
  - name: c
    engine: react
    params: {q: {type: string}}
    prompt: "回答 {q}"
skills:
  - name: via-mount
    steps: [{name: s, use: "components/c", args: '{"q":"x"}'}]
`,
		"namespaces/plain.yaml": `
components:
  - name: inherit
    engine: react
    params: {q: {type: string}}
    prompt: "回答 {q}"
skills:
  - name: via-agent
    steps: [{name: s, use: "components/inherit", args: '{"q":"x"}'}]
`,
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := BuildApp(context.Background(), spec, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	check := func(agentName, skillRef, want string) {
		t.Helper()
		sk, err := app.AgentMounts[agentName].Get(skillRef)
		if err != nil {
			t.Fatalf("%s/%s: %v", agentName, skillRef, err)
		}
		out, err := capability.Invoke(context.Background(), sk, `{}`)
		if err != nil {
			t.Fatalf("%s/%s: %v", agentName, skillRef, err)
		}
		if out != want {
			t.Fatalf("%s/%s: got %q, want %q", agentName, skillRef, out, want)
		}
	}
	check("a", "cap://skill/over/via-mount", "mount-model")  // per-mount 指定最高优
	check("a", "cap://skill/plain/via-agent", "agent-model") // 无 mount 指定 → agent 自己
	check("b", "cap://skill/plain/via-agent", "app-model")   // 无 agent model → app
}

// TestNamespaceModelRejected 验证 namespace 不能自指 model(能力不可自指)。
func TestNamespaceModelRejected(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: m}}
agents: [agents/a.yaml]
`,
		"agents/a.yaml": "namespaces: [../namespaces/bad.yaml]\n",
		"namespaces/bad.yaml": `
model: {provider: marker, config: {resp: ns-model}}
skills:
  - name: x
    steps: [{name: s, use: "model", prompt: "hi"}]
`,
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildApp(context.Background(), spec, BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "model cannot be declared here") {
		t.Fatalf("expect namespace model rejection, got %v", err)
	}
}

func TestNamespaceSourceShared(t *testing.T) {
	setupAppTestFakes()
	syncCount.Store(0)
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: m}}
agents: [agents/x.yaml, agents/y.yaml]
`,
		"agents/x.yaml": "namespaces: [../namespaces/shared.yaml]\n",
		"agents/y.yaml": "namespaces: [../namespaces/shared.yaml]\n",
		"namespaces/shared.yaml": `
sources:
  - {name: cnt, type: countsrc}
skills:
  - name: ping
    steps: [{name: s, use: "tools/cnt/ping"}]
`,
	})
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildApp(context.Background(), spec, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	// 两个 agent 各自实例化 namespace,但源只 Sync 一次(连接共享)
	if n := syncCount.Load(); n != 1 {
		t.Fatalf("source synced %d times, want 1 (cache shared across agents)", n)
	}
}

func TestStepDefaultsChain(t *testing.T) {
	setupAppTestFakes()
	appPath := writeTree(t, map[string]string{
		"app.yaml": `
model: {provider: marker, config: {resp: m}}
agents: [agents/a.yaml]
`,
		"agents/a.yaml": `
step_defaults: {retry: 2}
namespaces: [../namespaces/flaky.yaml]
`,
		"namespaces/flaky.yaml": `
sources:
  - {name: svc, type: flakysrc}
skills:
  - name: robust
    steps: [{name: s, use: "tools/svc/wobble"}]   # 步骤未声明 retry → agent 默认 2
`,
	})
	registerFlakySource()
	flakyCalls.Store(0)
	spec, err := LoadApp(appPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := BuildApp(context.Background(), spec, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sk, err := app.AgentMounts["a"].Get("cap://skill/flaky/robust")
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), sk, `{}`)
	if err != nil || out != "ok" {
		t.Fatalf("step_retry default should retry to success, got %q %v (calls=%d)", out, err, flakyCalls.Load())
	}
	if flakyCalls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (retry 2 from agent defaults)", flakyCalls.Load())
	}
}

var (
	registerFlaky sync.Once
	flakyCalls    atomic.Int32
)

func registerFlakySource() {
	registerFlaky.Do(func() {
		source.Register("flakysrc", func(_ context.Context, name string, _ map[string]any) (source.Source, error) {
			c := capability.New(capability.Meta{
				Ref: capability.Ref{Kind: "tool", Domain: name, Name: "wobble"},
			}, func(_ context.Context, in string) (string, error) {
				if flakyCalls.Add(1) < 3 {
					return "", errTransientTest
				}
				return "ok", nil
			})
			return source.Static(name, c), nil
		})
	})
}

var errTransientTest = &transientTestErr{}

type transientTestErr struct{}

func (*transientTestErr) Error() string { return "wobble failed" }
