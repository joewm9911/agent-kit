// Package docker 是 agent-kit 官方的 docker 执行沙箱:在一次性、加固的
// 容器里跑脚本(无网络、只读根、tmpfs、限内存/CPU/PID、丢权限、禁提权)。
// 空导入本包即注册 "docker" 沙箱,config 以 tool.sandbox: docker 或 app 级
// exec.default_sandbox: docker 引用。核心框架不含沙箱(隔离交部署),本包是
// impl 层的按需实现(对齐 impl/store/redis);需本机装 docker,脚本运行时
// 与依赖预装在镜像里(见 examples/exec-runtime/Dockerfile)。
package docker

import (
	"bytes"
	"context"
	"fmt"
	osexec "os/exec"
	"time"

	"github.com/joewm9911/agent-kit/protocol/exec"
)

func init() {
	exec.RegisterSandbox("docker", func(conf map[string]any) (exec.Sandbox, error) {
		img, _ := conf["image"].(string)
		if img == "" {
			return nil, fmt.Errorf("docker sandbox: image is required")
		}
		d := &docker{image: img, network: "none", memory: "512m"}
		if v, ok := conf["network"].(string); ok && v != "" {
			d.network = v
		}
		if v, ok := conf["memory"].(string); ok && v != "" {
			d.memory = v
		}
		if v, ok := conf["runtime"].(string); ok {
			d.runtime = v // exectool 按脚本类型注入(python/node/bash/sh)
		}
		if v, ok := conf["timeout"].(string); ok && v != "" {
			t, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("docker sandbox: timeout: %w", err)
			}
			d.timeout = t
		}
		return d, nil
	})
}

// docker 在一次性加固容器里跑一种 runtime 的脚本。脚本经解释器的
// "读脚本" 形式传入(python3 -c / node -e / bash -c),args 作位置参数。
type docker struct {
	image   string
	network string
	memory  string
	runtime string // python(默认)| node | bash | sh
	timeout time.Duration
}

// interp 返回容器内解释器读脚本的命令,以及 args 前是否需 $0 占位
// (bash/sh 的 -c 之后 args 从 $1 起,需占位;python/node 不需)。
func interp(runtime string) (argv []string, needArg0 bool) {
	switch runtime {
	case "node":
		return []string{"node", "-e"}, false
	case "bash":
		return []string{"bash", "-c"}, true
	case "sh":
		return []string{"sh", "-c"}, true
	default: // python 及未知
		return []string{"python3", "-c"}, false
	}
}

func (d *docker) Exec(ctx context.Context, script string, args []string) (string, error) {
	if d.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	argv := []string{
		"run", "--rm", "-i",
		"--network=" + d.network,
		"--memory=" + d.memory,
		"--cpus=1",
		"--pids-limit=128",
		"--read-only",
		"--tmpfs=/tmp:size=64m",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"-w", "/tmp",
		d.image,
	}
	interpArgv, needArg0 := interp(d.runtime)
	argv = append(argv, interpArgv...)
	argv = append(argv, script)
	if needArg0 {
		argv = append(argv, "_") // $0 占位,args 从 $1 起
	}
	argv = append(argv, args...)

	var out bytes.Buffer
	cmd := osexec.CommandContext(ctx, "docker", argv...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// 非零退出/超时作结果回传,不中断循环。
		return fmt.Sprintf("exit error: %v\n%s", err, out.String()), nil
	}
	return out.String(), nil
}
