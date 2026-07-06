// Package exectool 把"执行一段脚本"声明为 Source:一段配置列几个工具、
// 各绑一种运行时(python/node/bash/sh),每个成为一个
// cap://tool/<源名>/<工具名> 能力,入参只有 script + args。
//
// 执行引擎三级解析(见 runner):
//
//	tool.sandbox 指定  → 用注册的 Sandbox(docker/远程/WASM)
//	否则 tool.command  → exec 该命令(命令里可包一层沙箱,如 docker/firejail)
//	否则               → 内置模板(python3 -c / node -e / bash -c / sh -c),宿主直跑
//
// 能力默认 Risk=Dangerous(跑代码),不入目录除非 catalog.max_risk: dangerous。
// **框架不含任何沙箱实现**——隔离交给部署(容器/VM 里跑,或 command/sandbox
// 包一层)。非零退出/超时作结果回传,不返 error,让大脑看到报错自行调整。
//
//	tools:
//	  - name: exec
//	    type: exec
//	    config:
//	      timeout: 30s
//	      tools:
//	        - {name: python, runtime: python, sandbox: docker, sandbox_config: {...}}
//	        - {name: node,   runtime: node}
//	        - {name: bash,   runtime: bash, command: ["firejail","--quiet","bash","-c"]}
//	        - {name: sh,     runtime: sh}
package exectool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/protocol/exec"
	"github.com/joewm9911/agent-kit/protocol/source"
)

func init() {
	source.Register("exec", func(_ context.Context, name string, conf map[string]any) (source.Source, error) {
		var cfg SourceConfig
		if err := decode.Config(conf, &cfg); err != nil {
			return nil, err
		}
		return New(name, cfg)
	})
}

// 脚本执行沙箱协议(Sandbox/SandboxFactory/RegisterSandbox)已上浮基座 exec 包
// (可扩展接缝);这里的 source 只负责把脚本包成能力并选引擎。

// ---- 内置运行时模板(宿主直跑)----

// builtinTemplates 是各脚本类型的默认解释器调用。
var builtinTemplates = map[string][]string{
	"bash":   {"bash", "-c"},
	"sh":     {"sh", "-c"},
	"python": {"python3", "-c"},
	"node":   {"node", "-e"},
}

// needsArg0 报告该运行时的 -c 之后是否需要一个 $0 占位,args 才从 $1 起
// (bash/sh 需要;python/node 的 -c/-e 之后位置参数直接进 argv,不需要)。
func needsArg0(runtime string) bool { return runtime == "bash" || runtime == "sh" }

// ---- 配置 ----

// SourceConfig 声明一个 exec 源下的一批脚本执行工具。
type SourceConfig struct {
	Timeout string       `json:"timeout"` // 命令/模板执行的墙钟兜底(如 30s),空=无
	Workdir string       `json:"workdir"` // 命令/模板路径的工作目录(skillpack 绑定包目录用);engine 路径由引擎自管
	Tools   []ToolConfig `json:"tools"`
	// 装配层默认沙箱策略(app 级 exec 块注入):工具未显式配 sandbox/command
	// 时回落到它;require_sandbox 时禁止回落到内置模板(宿主直跑)。
	DefaultSandbox     string         `json:"default_sandbox"`
	DefaultSandboxConf map[string]any `json:"default_sandbox_config"`
	RequireSandbox     bool           `json:"require_sandbox"`
}

// ToolConfig 声明一个脚本执行工具。
type ToolConfig struct {
	Name        string         `json:"name"`
	Runtime     string         `json:"runtime"`     // bash | sh | python | node(决定 $0 占位与默认模板)
	Description string         `json:"description"` // 给模型看的说明,空=按 runtime 生成
	Command     []string       `json:"command"`     // 覆盖内置模板(命令里包沙箱);与 sandbox 互斥
	Sandbox     string         `json:"sandbox"`     // 注册的沙箱名;优先于 command/模板
	SandboxConf map[string]any `json:"sandbox_config"`
	Timeout     string         `json:"timeout"` // 覆盖源级 timeout
	Risk        string         `json:"risk"`    // 默认 dangerous
}

// New 从配置构造 exec 源。装配期解析引擎、校验运行时,fail fast。
func New(name string, cfg SourceConfig) (source.Source, error) {
	srcTimeout, err := parseTimeout(cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("exec source %s: timeout: %w", name, err)
	}
	def := sandboxDefault{name: cfg.DefaultSandbox, conf: cfg.DefaultSandboxConf, require: cfg.RequireSandbox}
	caps := make([]capability.Capability, 0, len(cfg.Tools))
	for _, tc := range cfg.Tools {
		c, err := newTool(name, tc, srcTimeout, cfg.Workdir, def)
		if err != nil {
			return nil, fmt.Errorf("exec source %s: %w", name, err)
		}
		caps = append(caps, c)
	}
	return source.Static(name, caps...), nil
}

// sandboxDefault 是装配层注入的默认沙箱策略(非 exec 包全局单例:每次 New
// 从配置解析后随构造下传,消费方持有,不读全局)。
type sandboxDefault struct {
	name    string
	conf    map[string]any
	require bool
}

func newTool(srcName string, tc ToolConfig, srcTimeout time.Duration, workdir string, def sandboxDefault) (capability.Capability, error) {
	if tc.Name == "" || tc.Runtime == "" {
		return nil, fmt.Errorf("tool: name and runtime are required")
	}
	if tc.Sandbox != "" && len(tc.Command) > 0 {
		return nil, fmt.Errorf("tool %s: sandbox 与 command 互斥", tc.Name)
	}

	// 风险默认 dangerous(跑代码)。
	riskStr := tc.Risk
	if riskStr == "" {
		riskStr = "dangerous"
	}
	risk, err := capability.ParseRisk(riskStr)
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", tc.Name, err)
	}

	timeout := srcTimeout
	if tc.Timeout != "" {
		if timeout, err = parseTimeout(tc.Timeout); err != nil {
			return nil, fmt.Errorf("tool %s: timeout: %w", tc.Name, err)
		}
	}

	// 解析执行器(四级):工具级 sandbox > 工具级 command > 装配层默认 sandbox
	// > 内置模板(宿主直跑)。require_sandbox 时禁用最后一级——无沙箱即
	// fail fast,脚本裸跑架构上不可能。
	sbName, sbConf := tc.Sandbox, tc.SandboxConf
	if sbName == "" && len(tc.Command) == 0 && def.name != "" {
		sbName, sbConf = def.name, def.conf
	}
	var sb exec.Sandbox
	var cmdTmpl []string
	switch {
	case sbName != "":
		f, ok := exec.Lookup(sbName)
		if !ok {
			return nil, fmt.Errorf("tool %s: unknown sandbox %q(需先 RegisterSandbox)", tc.Name, sbName)
		}
		// runtime 注入沙箱配置:沙箱据此选容器内解释器(docker 等据 runtime
		// 决定跑 python/node/bash)。
		if sb, err = f(withRuntime(sbConf, tc.Runtime)); err != nil {
			return nil, fmt.Errorf("tool %s: sandbox %q: %w", tc.Name, sbName, err)
		}
	case len(tc.Command) > 0:
		cmdTmpl = tc.Command
	default:
		if def.require {
			return nil, fmt.Errorf("tool %s: require_sandbox 已开但该工具无沙箱可用(既无工具级 sandbox,也无 exec.default_sandbox)——拒绝宿主直跑", tc.Name)
		}
		t, ok := builtinTemplates[tc.Runtime]
		if !ok {
			return nil, fmt.Errorf("tool %s: 未知 runtime %q 且未提供 command/sandbox(内置:bash|sh|python|node)", tc.Name, tc.Runtime)
		}
		cmdTmpl = t
	}

	desc := tc.Description
	if desc == "" {
		desc = "用 " + tc.Runtime + " 执行一段脚本并返回输出。script=脚本内容,args=空格分隔参数。"
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: srcName, Name: tc.Name},
		Description: desc,
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"script": {Type: schema.String, Desc: "脚本内容", Required: true},
			"args":   {Type: schema.String, Desc: "参数,空格分隔(可空)"},
		}),
		Risk: risk,
	}
	r := &runner{runtime: tc.Runtime, sandbox: sb, command: cmdTmpl, timeout: timeout, workdir: workdir}
	return capability.New(meta, r.run), nil
}

// ---- 执行 ----

type runner struct {
	runtime string
	sandbox exec.Sandbox // 非 nil = 走沙箱
	command []string     // 非空 = 走命令模板(sandbox 为 nil 时)
	timeout time.Duration
	workdir string // 非空 = 命令/模板在该目录下执行(脚本可读同目录文件)
}

type execArgs struct {
	Script string `json:"script"`
	Args   string `json:"args"`
}

func (r *runner) run(ctx context.Context, argsJSON string) (string, error) {
	var in execArgs
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
	}
	if strings.TrimSpace(in.Script) == "" {
		return "script is required", nil
	}
	args := strings.Fields(in.Args)

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	// 沙箱路径:隔离/资源限制由沙箱负责。
	if r.sandbox != nil {
		return r.sandbox.Exec(ctx, in.Script, args)
	}

	// 命令/模板路径:拼 argv 后宿主执行(命令里可能已包一层沙箱)。
	argv := append([]string{}, r.command...)
	argv = append(argv, in.Script)
	if needsArg0(r.runtime) {
		argv = append(argv, "_") // $0 占位,args 从 $1 起
	}
	argv = append(argv, args...)

	var out bytes.Buffer
	cmd := osexec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.workdir // 空 = 进程 cwd
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// 非零退出/超时作结果回传,不中断循环。
		return fmt.Sprintf("exit error: %v\n%s", err, out.String()), nil
	}
	return out.String(), nil
}

func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// withRuntime 把工具的 runtime 并入沙箱配置(不覆盖已显式声明的 runtime),
// 供沙箱实现按脚本类型选解释器。
func withRuntime(conf map[string]any, runtime string) map[string]any {
	out := map[string]any{"runtime": runtime}
	for k, v := range conf {
		out[k] = v
	}
	return out
}
