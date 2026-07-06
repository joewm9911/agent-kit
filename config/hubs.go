// hubs.go:技能装配的按名解析环境(eino AgentHub/ModelHub 的本地等价物)。
// skillpack frontmatter 的 `agent:`/`model:` 字段据此解析:agent 注册表在
// 全部 agent 装配完成后回填(技能装配早于 agent,查找延迟到调用期,名字
// 合法性用"已声明 agent 名"在装配期校验);具名模型懒构建 + 缓存,Ring 0
// 包装与其他专属模型同源。
package config

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// NamedModelConfig 声明一个具名模型(顶层 models: 块),供 skillpack
// frontmatter `model:` 按名引用。
type NamedModelConfig struct {
	Name     string         `yaml:"name"`
	Provider string         `yaml:"provider"`
	Config   map[string]any `yaml:"config"`
}

// skillHubs 是技能装配的按名解析环境,随 deps 下传到 buildSkillpack。
type skillHubs struct {
	agents *agentHub
	known  map[string]bool // 已声明的 agent 名(装配期校验 frontmatter agent:)
	models func(ctx context.Context, name string) (einomodel.ToolCallingChatModel, error)
}

func newSkillHubs(models []NamedModelConfig, retry loop.RetryConfig, agentNames []string) (*skillHubs, error) {
	idx := map[string]NamedModelConfig{}
	for _, mc := range models {
		if mc.Name == "" || mc.Provider == "" {
			return nil, fmt.Errorf("models: 每个具名模型需要 name 与 provider")
		}
		if _, dup := idx[mc.Name]; dup {
			return nil, fmt.Errorf("models: 具名模型 %q 重复声明", mc.Name)
		}
		idx[mc.Name] = mc
	}
	known := map[string]bool{}
	for _, n := range agentNames {
		known[n] = true
	}

	var mu sync.Mutex
	cache := map[string]einomodel.ToolCallingChatModel{}
	resolve := func(ctx context.Context, name string) (einomodel.ToolCallingChatModel, error) {
		mu.Lock()
		defer mu.Unlock()
		if m, ok := cache[name]; ok {
			return m, nil
		}
		mc, ok := idx[name]
		if !ok {
			declared := make([]string, 0, len(idx))
			for n := range idx {
				declared = append(declared, n)
			}
			sort.Strings(declared)
			return nil, fmt.Errorf("具名模型 %q 未声明(顶层 models: 块;已声明:%s)", name, strings.Join(declared, ", "))
		}
		m, err := model.Build(ctx, mc.Provider, mc.Config)
		if err != nil {
			return nil, err
		}
		wrapped := loop.BudgetModel(loop.RetryModel(m, retry)) // 质量守卫在循环装配层(ReviewModel)
		cache[name] = wrapped
		return wrapped, nil
	}
	return &skillHubs{agents: newAgentHub(), known: known, models: resolve}, nil
}

// agentHub 是"按名查已装配 agent"的注册表:init 后回填、运行期只读。
type agentHub struct {
	mu sync.RWMutex
	m  map[string]capability.Capability
}

func newAgentHub() *agentHub { return &agentHub{m: map[string]capability.Capability{}} }

func (h *agentHub) add(name string, c capability.Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[name] = c
}

func (h *agentHub) lookup(name string) (capability.Capability, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.m[name]
	return c, ok
}
