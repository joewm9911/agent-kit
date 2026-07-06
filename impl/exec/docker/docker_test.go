package docker

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	agexec "github.com/joewm9911/agent-kit/protocol/exec"
)

// TestInterp 验证 runtime → 容器内解释器命令映射(不需 docker)。
func TestInterp(t *testing.T) {
	cases := []struct {
		rt       string
		argv     string
		needArg0 bool
	}{
		{"python", "python3 -c", false},
		{"node", "node -e", false},
		{"bash", "bash -c", true},
		{"sh", "sh -c", true},
		{"unknown", "python3 -c", false},
	}
	for _, c := range cases {
		argv, arg0 := interp(c.rt)
		if strings.Join(argv, " ") != c.argv || arg0 != c.needArg0 {
			t.Fatalf("interp(%q) = %v %v, want %q %v", c.rt, argv, arg0, c.argv, c.needArg0)
		}
	}
}

// TestFactory 验证注册与配置解析(image 必填、默认值、runtime 注入)。
func TestFactory(t *testing.T) {
	f, ok := agexec.Lookup("docker")
	if !ok {
		t.Fatal("docker sandbox not registered")
	}
	if _, err := f(map[string]any{}); err == nil {
		t.Fatal("image required")
	}
	sb, err := f(map[string]any{"image": "img", "runtime": "node", "network": "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	d := sb.(*docker)
	if d.image != "img" || d.runtime != "node" || d.network != "bridge" || d.memory != "512m" {
		t.Fatalf("factory parse: %+v", d)
	}
}

// TestDockerExec 真跑一次(docker 可用时):python 脚本回显。
func TestDockerExec(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	f, _ := agexec.Lookup("docker")
	sb, err := f(map[string]any{"image": "python:3.12-slim", "runtime": "python"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := sb.Exec(context.Background(), "print(6*7)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("docker exec: %q", out)
	}
}
