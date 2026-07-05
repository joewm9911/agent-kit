package exectool

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/exec"
)

// invoke 取源里名为 name 的能力并调用。
func invoke(t *testing.T, src interface {
	Sync(context.Context) ([]capability.Capability, error)
}, name, argsJSON string) string {
	t.Helper()
	caps, err := src.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.Meta().Ref.Name == name {
			out, err := capability.Invoke(context.Background(), c, argsJSON)
			if err != nil {
				t.Fatalf("invoke %s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("cap %q not found", name)
	return ""
}

// TestExecTemplateRuntimes 覆盖内置模板路径:sh/bash 的 $0 占位使 args 从
// $1 起、ref 归属、非零退出作结果回传。用 sh/bash(必定存在),不依赖 python/node。
func TestExecTemplateRuntimes(t *testing.T) {
	src, err := New("exec", SourceConfig{
		Timeout: "10s",
		Tools: []ToolConfig{
			{Name: "sh", Runtime: "sh"},
			{Name: "bash", Runtime: "bash"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	caps, _ := src.Sync(context.Background())
	if len(caps) != 2 {
		t.Fatalf("want 2 caps, got %d", len(caps))
	}
	// ref 归属:cap://tool/exec/<name>,默认 dangerous
	if ref := caps[0].Meta().Ref.String(); ref != "cap://tool/exec/sh" {
		t.Fatalf("ref = %s", ref)
	}
	if caps[0].Meta().Risk != capability.RiskDangerous {
		t.Fatalf("risk = %v, want dangerous", caps[0].Meta().Risk)
	}

	// $0 占位:args 从 $1 起 → echo "$1" 打印第一个参数
	out := invoke(t, src, "sh", `{"script":"echo hello-$1","args":"world"}`)
	if strings.TrimSpace(out) != "hello-world" {
		t.Fatalf("sh args: got %q", out)
	}
	out = invoke(t, src, "bash", `{"script":"echo $1$2","args":"a b"}`)
	if strings.TrimSpace(out) != "ab" {
		t.Fatalf("bash args: got %q", out)
	}

	// 非零退出:作结果回传(不返 error),含 exit 信息
	out = invoke(t, src, "sh", `{"script":"echo boom >&2; exit 3"}`)
	if !strings.Contains(out, "exit error") || !strings.Contains(out, "boom") {
		t.Fatalf("nonzero exit should return result with stderr: %q", out)
	}

	// 空 script:友好提示,不执行
	if out := invoke(t, src, "sh", `{"script":"  "}`); !strings.Contains(out, "required") {
		t.Fatalf("empty script: got %q", out)
	}
}

// fakeEngine 是测试用的自定义引擎:不起进程,回显收到的脚本与参数。
type fakeEngine struct{ tag string }

func (e fakeEngine) Exec(_ context.Context, script string, args []string) (string, error) {
	return e.tag + ":" + script + "|" + strings.Join(args, ","), nil
}

var registerFake sync.Once

// TestExecCustomEngine 验证 engine 路径:注册的引擎替代进程执行,拿到脚本与参数。
func TestExecCustomEngine(t *testing.T) {
	registerFake.Do(func() {
		exec.RegisterEngine("fake", func(conf map[string]any) (exec.Engine, error) {
			tag, _ := conf["tag"].(string)
			return fakeEngine{tag: tag}, nil
		})
	})

	src, err := New("exec", SourceConfig{
		Tools: []ToolConfig{
			{Name: "py", Runtime: "python", Engine: "fake", EngineConf: map[string]any{"tag": "T"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := invoke(t, src, "py", `{"script":"print(1)","args":"a b"}`)
	if out != "T:print(1)|a,b" {
		t.Fatalf("engine path: got %q", out)
	}
}

// TestExecValidation 覆盖装配期校验(fail fast)。
func TestExecValidation(t *testing.T) {
	cases := []struct {
		name string
		tc   ToolConfig
		want string
	}{
		{"unknown-runtime-no-fallback", ToolConfig{Name: "x", Runtime: "ruby"}, "未知 runtime"},
		{"engine-and-command", ToolConfig{Name: "x", Runtime: "bash", Engine: "fake", Command: []string{"bash", "-c"}}, "互斥"},
		{"unknown-engine", ToolConfig{Name: "x", Runtime: "python", Engine: "ghost"}, "unknown engine"},
		{"missing-name", ToolConfig{Runtime: "sh"}, "required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New("exec", SourceConfig{Tools: []ToolConfig{c.tc}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

// TestExecCustomCommand 验证 command 覆盖:用自定义命令代替内置模板。
func TestExecCustomCommand(t *testing.T) {
	// 用 `sh -c` 当"自定义命令",验证 command 路径 + $0 占位仍按 runtime 生效
	src, err := New("exec", SourceConfig{
		Tools: []ToolConfig{
			{Name: "wrapped", Runtime: "sh", Command: []string{"sh", "-c"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := invoke(t, src, "wrapped", `{"script":"echo cmd-$1","args":"ok"}`)
	if strings.TrimSpace(out) != "cmd-ok" {
		t.Fatalf("custom command: got %q", out)
	}
}
