// Package exec 定义脚本执行引擎的协议(可扩展接缝):一段脚本怎么被
// 隔离执行,由 agent-kit 的使用方实现并注册(docker/远程/WASM/…),框架
// 不含任何沙箱实现。脚本执行工具(impl/source/exec 等)从这里取引擎。
package exec

import (
	"context"
	"fmt"
	"sync"
)

// Engine 是一种脚本类型的执行后端。给定脚本正文与参数,回传输出;
// 隔离/资源限制由实现负责,框架不做假设。
type Engine interface {
	Exec(ctx context.Context, script string, args []string) (string, error)
}

// EngineFactory 按 engine_config 构造引擎实例。
type EngineFactory func(conf map[string]any) (Engine, error)

var (
	mu      sync.RWMutex
	engines = map[string]EngineFactory{}
)

// RegisterEngine 注册自定义执行引擎(docker/远程/WASM/…),config 里以
// tool.engine 引用。空导入注册即可。
func RegisterEngine(name string, f EngineFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := engines[name]; ok {
		panic(fmt.Sprintf("exec: engine %q already registered", name))
	}
	engines[name] = f
}

// Lookup 返回已注册的引擎工厂。
func Lookup(name string) (EngineFactory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := engines[name]
	return f, ok
}
