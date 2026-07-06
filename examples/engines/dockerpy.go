// Package engines 是 exectool 自定义执行引擎的示例:一个 docker 沙箱引擎。
// 空导入本包即注册 "docker" 引擎,config 里以 tool.engine: docker 引用。
//
// 这是"框架不含沙箱、隔离交部署"的落地示范——docker 隔离在这里,agent-kit
// 只提供 exectool 的引擎注册点。需要本机装了 docker 才能真正执行。
package engines

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
			return nil, fmt.Errorf("docker engine: image is required")
		}
		e := &dockerPy{image: img, network: "none", memory: "512m"}
		if v, ok := conf["network"].(string); ok && v != "" {
			e.network = v
		}
		if v, ok := conf["memory"].(string); ok && v != "" {
			e.memory = v
		}
		if v, ok := conf["timeout"].(string); ok && v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("docker engine: timeout: %w", err)
			}
			e.timeout = d
		}
		return e, nil
	})
}

// dockerPy 在一次性容器里跑 python 脚本:无网络、只读根、tmpfs、限内存/CPU、
// 丢权限。脚本经 python3 -c 传入,args 作位置参数。
type dockerPy struct {
	image   string
	network string
	memory  string
	timeout time.Duration
}

func (e *dockerPy) Exec(ctx context.Context, script string, args []string) (string, error) {
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	argv := append([]string{
		"run", "--rm", "-i",
		"--network=" + e.network,
		"--memory=" + e.memory,
		"--cpus=1",
		"--pids-limit=128",
		"--read-only",
		"--tmpfs=/tmp:size=64m",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"-w", "/tmp",
		e.image,
		"python3", "-c", script,
	}, args...)

	var out bytes.Buffer
	cmd := osexec.CommandContext(ctx, "docker", argv...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// 非零退出/超时作结果回传,不中断循环。
		return fmt.Sprintf("exit error: %v\n%s", err, out.String()), nil
	}
	return out.String(), nil
}
