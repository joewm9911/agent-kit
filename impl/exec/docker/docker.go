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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	osexec "os/exec"
	"time"

	"github.com/joewm9911/agent-kit/protocol/exec"
)

func init() {
	exec.RegisterSandbox("docker", func(conf map[string]any) (exec.Sandbox, error) {
		img, err := confString(conf, "image")
		if err != nil {
			return nil, err
		}
		if img == "" {
			return nil, fmt.Errorf("docker sandbox: image is required")
		}
		d := &docker{image: img, network: "none", memory: "512m"}
		if v, err := confString(conf, "network"); err != nil {
			return nil, err
		} else if v != "" {
			d.network = v
		}
		if v, err := confString(conf, "memory"); err != nil {
			return nil, err
		} else if v != "" {
			d.memory = v
		}
		if v, err := confString(conf, "runtime"); err != nil {
			return nil, err
		} else {
			d.runtime = v // exectool 按脚本类型注入(python/node/bash/sh)
		}
		// timeout 接受时长字符串("30s")或裸秒数——与框架共享词汇
		// (capability.Duration)一致。类型不符必须报错:安全组件的资源
		// 限制被静默忽略(timeout: 30 曾因裸断言直接丢弃 = 无超时)。
		switch v := conf["timeout"].(type) {
		case nil:
		case string:
			if v != "" {
				t, err := time.ParseDuration(v)
				if err != nil {
					return nil, fmt.Errorf("docker sandbox: timeout: %w", err)
				}
				d.timeout = t
			}
		case int:
			d.timeout = time.Duration(v) * time.Second
		case float64:
			d.timeout = time.Duration(v * float64(time.Second))
		default:
			return nil, fmt.Errorf("docker sandbox: timeout must be a duration string or seconds, got %T", v)
		}
		return d, nil
	})
}

// confString 取字符串配置项;存在但不是字符串必须报错(静默忽略会让
// 加固参数悄悄回落默认)。
func confString(conf map[string]any, key string) (string, error) {
	v, ok := conf[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("docker sandbox: %s must be a string, got %T", key, v)
	}
	return s, nil
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

// maxOutputBytes 是宿主侧输出缓冲上限:--memory 只限容器,刷屏脚本
// (yes 循环)会先打爆宿主内存;超限截断,循环层 8000 字截断闸再兜一层。
const maxOutputBytes = 1 << 20 // 1MB

// boundedBuf 是带上限的输出缓冲,超限丢弃后续写入(保留前段)。
type boundedBuf struct {
	buf     bytes.Buffer
	clipped bool
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	if room := maxOutputBytes - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.clipped = true
		} else {
			b.buf.Write(p)
		}
	} else {
		b.clipped = true
	}
	return len(p), nil // 对 cmd 恒报成功,丢弃而非反压
}

func (b *boundedBuf) String() string {
	if b.clipped {
		return b.buf.String() + "\n...[output clipped at 1MB]"
	}
	return b.buf.String()
}

func (d *docker) Exec(ctx context.Context, script string, args []string) (string, error) {
	if d.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	// 容器命名:超时/取消时 CommandContext 杀的只是本机 docker CLI,容器
	// 本体还在 dockerd 里跑(--rm 要等退出,死循环脚本永不退出)——必须
	// 按名强删,否则每次超时泄漏一个烧 CPU 的容器。
	var nb [6]byte
	_, _ = rand.Read(nb[:])
	name := "agentkit-exec-" + hex.EncodeToString(nb[:])

	argv := []string{
		"run", "--rm", "-i",
		"--name=" + name,
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

	var out boundedBuf
	cmd := osexec.CommandContext(ctx, "docker", argv...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		// 兜底回收:CLI 被杀不等于容器退出。--rm 成功路径下容器已不在,
		// rm -f 幂等无害;用独立短超时 ctx,不受已过期的执行 ctx 拖累。
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = osexec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
		rmCancel()

		// 基建失败与脚本失败要分开:125/126/127 是 docker 层错误(daemon
		// 挂了/镜像缺失/不可执行),模型改写脚本永远修不好,必须以错误
		// 上抛让运维看见;脚本自身的非零退出/超时才作结果回传自纠错。
		var ee *osexec.ExitError
		if errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 125, 126, 127:
				return "", fmt.Errorf("docker sandbox infrastructure error (exit %d): %s", ee.ExitCode(), out.String())
			}
		}
		return fmt.Sprintf("exit error: %v\n%s", err, out.String()), nil
	}
	return out.String(), nil
}
