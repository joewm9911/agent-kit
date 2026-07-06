// Package exec 定义脚本执行沙箱的协议(可扩展接缝):脚本在哪个隔离环境
// 执行,由使用方实现并注册(docker/远程/WASM/…),核心不含沙箱实现(官方
// docker 实现见 impl/exec/docker)。命名用 sandbox 而非 engine,与编排引擎
// (runtime/engine)区分。
package exec

import (
	"context"
	"fmt"
	"sync"
)

// Sandbox 是一种脚本类型的隔离执行后端。给定脚本正文与参数,回传输出;
// 隔离/资源限制由实现负责,框架不做假设。
type Sandbox interface {
	Exec(ctx context.Context, script string, args []string) (string, error)
}

// SandboxFactory 按 sandbox_config 构造沙箱实例。
type SandboxFactory func(conf map[string]any) (Sandbox, error)

var (
	mu        sync.RWMutex
	sandboxes = map[string]SandboxFactory{}
)

// RegisterSandbox 注册自定义执行沙箱(docker/远程/WASM/…),config 里以
// tool.sandbox 或 app 级 exec.default_sandbox 引用。空导入注册即可。
func RegisterSandbox(name string, f SandboxFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := sandboxes[name]; ok {
		panic(fmt.Sprintf("exec: sandbox %q already registered", name))
	}
	sandboxes[name] = f
}

// Lookup 返回已注册的沙箱工厂。
func Lookup(name string) (SandboxFactory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := sandboxes[name]
	return f, ok
}
