package config

import (
	"reflect"
	"testing"
)

// TestExecInjectInto 验证默认沙箱策略并入 exec source conf 的语义:
// 不覆盖已有键、require 无条件注入、空策略原样返回。
func TestExecInjectInto(t *testing.T) {
	base := map[string]any{"tools": []any{}}
	if got := (ExecConfig{}).injectInto(base); !reflect.DeepEqual(got, base) {
		t.Fatalf("empty policy must pass through: %v", got)
	}

	e := ExecConfig{DefaultSandbox: "docker", SandboxConfig: map[string]any{"image": "x"}}
	got := e.injectInto(map[string]any{"tools": 1})
	if got["default_sandbox"] != "docker" || got["tools"] != 1 {
		t.Fatalf("inject: %v", got)
	}
	if m, _ := got["default_sandbox_config"].(map[string]any); m["image"] != "x" {
		t.Fatalf("sandbox_config not injected: %v", got)
	}

	got = e.injectInto(map[string]any{"default_sandbox": "custom"})
	if got["default_sandbox"] != "custom" {
		t.Fatalf("must not override explicit key: %v", got)
	}

	got = (ExecConfig{RequireSandbox: true}).injectInto(map[string]any{})
	if got["require_sandbox"] != true {
		t.Fatalf("require_sandbox must be injected: %v", got)
	}
}
