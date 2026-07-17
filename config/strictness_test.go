package config

// 配置严格化回归(审计批 P1-B):枚举与旧键的 fail-fast。
// 词汇写错必须装配期报错,不能静默按默认走。

import (
	"context"
	"strings"
	"testing"
)

func buildFromYAML(t *testing.T, yml string) error {
	t.Helper()
	setupDriftFakes()
	cfg, err := Load(writeTree(t, map[string]string{"app.yaml": yml}))
	if err != nil {
		return err
	}
	_, err = Build(context.Background(), cfg, BuildOptions{})
	return err
}

func TestConfigEnumFailFast(t *testing.T) {
	cases := []struct {
		name, yml, want string
	}{
		{"record_tools", `
model: {provider: marker, config: {resp: hi}}
agents:
  - name: a
    session: {record_tools: fulll}
`, "summary|full|off"},
		{"approval_mode", `
model: {provider: marker, config: {resp: hi}}
agents:
  - name: a
    approval: {mode: automatic}
`, "auto|interactive|deny"},
		{"memory_write_scope", `
model: {provider: marker, config: {resp: hi}}
agents:
  - name: a
    memory: {scope: {write: global}}
`, "user|shared|session"},
		{"deliver_enum", `
model: {provider: marker, config: {resp: hi}}
namespaces:
  - name: ops
    subagents:
      - name: c
        prompt: "做事"
        deliver: attch
agents:
  - name: a
`, "attach|always|direct"},
		{"default_model_redirect", `
default_model: {provider: marker, config: {resp: hi}}
agents:
  - name: a
`, "renamed model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildFromYAML(t, tc.yml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// params 的 type 词汇写错必须装配期报错(此前 int/str 静默转 string)。
func TestParamsSchemaUnknownType(t *testing.T) {
	err := buildFromYAML(t, `
model: {provider: marker, config: {resp: hi}}
namespaces:
  - name: ops
    subagents:
      - name: c
        params:
          n: {type: int, required: true}
        prompt: "处理 {n}"
agents:
  - name: a
`)
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("want unknown-type error, got %v", err)
	}
}
