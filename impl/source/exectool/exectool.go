// Package exectool 把"执行一段脚本"声明为 Source:一段配置列几个工具、
// 各绑一种运行时(python/node/bash/sh),每个成为一个
// cap://tool/<源名>/<工具名> 能力,入参只有 script + args。
//
// 执行引擎三级解析(见 runner):
//
//	tool.engine 指定   → 用注册的 Engine(自定义沙箱/远程/WASM)
//	否则 tool.command  → exec 该命令(命令里可包一层沙箱,如 docker/firejail)
//	否则               → 内置模板(python3 -c / node -e / bash -c / sh -c),宿主直跑
//
// 能力默认 Risk=Dangerous(跑代码),不入目录除非 catalog.max_risk: dangerous。
// **框架不含任何沙箱实现**——隔离交给部署(容器/VM 里跑,或 command/engine
// 包一层)。非零退出/超时作结果回传,不返 error,让大脑看到报错自行调整。
//
//	tools:
//	  - name: exec
//	    type: exec
//	    config:
//	      timeout: 30s
//	      tools:
//	        - {name: python, runtime: python, engine: docker, engine_config: {...}}
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

// 脚本执行引擎协议(Engine/EngineFactory/RegisterEngine)已上浮基座 exec 包
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
	Tools   []ToolConfig `json:"tools"`
}

// ToolConfig 声明一个脚本执行工具。
type ToolConfig struct {
	Name        string         `json:"name"`
	Runtime     string         `json:"runtime"`     // bash | sh | python | node(决定 $0 占位与默认模板)
	Description string         `json:"description"` // 给模型看的说明,空=按 runtime 生成
	Command     []string       `json:"command"`     // 覆盖内置模板(命令里包沙箱);与 engine 互斥
	Engine      string         `json:"engine"`      // 注册的引擎名;优先于 command/模板
	EngineConf  map[string]any `json:"engine_config"`
	Timeout     string         `json:"timeout"` // 覆盖源级 timeout
	Risk        string         `json:"risk"`    // 默认 dangerous
}

// New 从配置构造 exec 源。装配期解析引擎、校验运行时,fail fast。
func New(name string, cfg SourceConfig) (source.Source, error) {
	srcTimeout, err := parseTimeout(cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("exec source %s: timeout: %w", name, err)
	}
	caps := make([]capability.Capability, 0, len(cfg.Tools))
	for _, tc := range cfg.Tools {
		c, err := newTool(name, tc, srcTimeout)
		if err != nil {
			return nil, fmt.Errorf("exec source %s: %w", name, err)
		}
		caps = append(caps, c)
	}
	return source.Static(name, caps...), nil
}

func newTool(srcName string, tc ToolConfig, srcTimeout time.Duration) (capability.Capability, error) {
	if tc.Name == "" || tc.Runtime == "" {
		return nil, fmt.Errorf("tool: name and runtime are required")
	}
	if tc.Engine != "" && len(tc.Command) > 0 {
		return nil, fmt.Errorf("tool %s: engine 与 command 互斥", tc.Name)
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

	// 解析执行器(三级):engine > command > 内置模板。装配期定死。
	var eng exec.Engine
	var cmdTmpl []string
	switch {
	case tc.Engine != "":
		f, ok := exec.Lookup(tc.Engine)
		if !ok {
			return nil, fmt.Errorf("tool %s: unknown engine %q(需先 RegisterEngine)", tc.Name, tc.Engine)
		}
		if eng, err = f(tc.EngineConf); err != nil {
			return nil, fmt.Errorf("tool %s: engine %q: %w", tc.Name, tc.Engine, err)
		}
	case len(tc.Command) > 0:
		cmdTmpl = tc.Command
	default:
		t, ok := builtinTemplates[tc.Runtime]
		if !ok {
			return nil, fmt.Errorf("tool %s: 未知 runtime %q 且未提供 command/engine(内置:bash|sh|python|node)", tc.Name, tc.Runtime)
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
	r := &runner{runtime: tc.Runtime, engine: eng, command: cmdTmpl, timeout: timeout}
	return capability.New(meta, r.run), nil
}

// ---- 执行 ----

type runner struct {
	runtime string
	engine  exec.Engine // 非 nil = 走引擎
	command []string    // 非空 = 走命令模板(engine 为 nil 时)
	timeout time.Duration
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

	// 引擎路径:隔离/资源限制由引擎负责。
	if r.engine != nil {
		return r.engine.Exec(ctx, in.Script, args)
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
