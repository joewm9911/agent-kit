// Package vectorstore 定义向量库后端的协议(可扩展接缝):真实向量库
// (qdrant/milvus/自定义)实现 eino Retriever 并在这里注册,由 vector 检索
// 工具源(impl/source/vector)按名字选用。框架不硬依赖任何向量库。
//
// 命名 vectorstore(而非 retriever)以避免与 eino 的 retriever 包选择器
// 冲突——它是"产出 retriever 的后端"。
package vectorstore

import (
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/eino/components/retriever"
)

// BackendFactory 按配置构造 eino Retriever(向量库客户端)。
type BackendFactory func(conf map[string]any) (retriever.Retriever, error)

var (
	mu       sync.RWMutex
	backends = map[string]BackendFactory{}
)

// RegisterBackend 注册检索后端(qdrant/milvus/自定义),实现方空导入即可
// 在配置里以 backend: <name> 引用。真实向量库由此接入,不在框架内硬依赖。
func RegisterBackend(name string, f BackendFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := backends[name]; ok {
		panic(fmt.Sprintf("vectorstore: backend %q already registered", name))
	}
	backends[name] = f
}

// New 按名字构造检索后端,空名默认 inmemory(词法保底)。
func New(name string, conf map[string]any) (retriever.Retriever, error) {
	if name == "" {
		name = "inmemory"
	}
	mu.RLock()
	f, ok := backends[name]
	mu.RUnlock()
	if !ok {
		names := make([]string, 0, len(backends))
		for k := range backends {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("vectorstore: unknown backend %q, registered: %v", name, names)
	}
	return f(conf)
}
