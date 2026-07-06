// exec.go:app 级脚本执行沙箱策略。default_sandbox 让所有 exec 能力
// (exectool 声明的、skillpack 自动生成的)不显式配就走同一沙箱;
// require_sandbox 禁止回落到宿主直跑。默认经装配层解析后注入进各 exec
// source 的 conf(不是 exec 包全局单例,守 DI 原则)。
package config

// ExecConfig 是 app 级脚本执行策略。
type ExecConfig struct {
	// DefaultSandbox 是未显式配 sandbox/command 的 exec 工具的回落沙箱名
	// (需 impl/exec/* 空导入注册,如 docker)。
	DefaultSandbox string `yaml:"default_sandbox"`
	// SandboxConfig 是默认沙箱的构造配置(如 docker 的 image/network/memory)。
	SandboxConfig map[string]any `yaml:"sandbox_config"`
	// RequireSandbox 开启后禁止宿主直跑:无沙箱可用的 exec 工具装配即 fail fast。
	RequireSandbox bool `yaml:"require_sandbox"`
}

// injectInto 把默认沙箱策略并入一个 exec source 的 conf map(不覆盖已有键),
// 供 source.New("exec", ...) 与 skillpack 的 exec 工具共用。
func (e ExecConfig) injectInto(conf map[string]any) map[string]any {
	if e.DefaultSandbox == "" && !e.RequireSandbox {
		return conf
	}
	out := map[string]any{}
	for k, v := range conf {
		out[k] = v
	}
	if e.DefaultSandbox != "" {
		if _, ok := out["default_sandbox"]; !ok {
			out["default_sandbox"] = e.DefaultSandbox
		}
		if _, ok := out["default_sandbox_config"]; !ok {
			out["default_sandbox_config"] = e.SandboxConfig
		}
	}
	if e.RequireSandbox {
		out["require_sandbox"] = true
	}
	return out
}
