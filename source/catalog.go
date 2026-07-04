package source

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/capability"
)

// Catalog 是能力目录:所有 source 的聚合与治理点。
// 冲突检测、优先级遮蔽、风险准入在入目录时完成;agent 用
// include/exclude 模式选品,短名撞车时自动升级为 namespace_name。
type Catalog struct {
	mu      sync.RWMutex
	entries map[string]entry // Ref.Key() -> entry
	maxRisk capability.Risk  // 准入上限,超过的能力不入目录
	logger  *slog.Logger
}

type entry struct {
	cap      capability.Capability
	source   string
	priority int
}

// NewCatalog 创建目录。maxRisk 是全局准入上限
// (通常 mutating;dangerous 能力默认被拒之门外)。
func NewCatalog(maxRisk capability.Risk, logger *slog.Logger) *Catalog {
	if logger == nil {
		logger = slog.Default()
	}
	return &Catalog{entries: map[string]entry{}, maxRisk: maxRisk, logger: logger}
}

// AddSource 同步一个 source 并把其能力纳入目录。
//   - required=true 且 Sync 失败 → 返回错误(agent 构建应失败);
//   - required=false 且 Sync 失败 → 记告警,跳过该源(工具面变小但可用);
//   - priority 解决跨源同 Key 冲突:高者胜(遮蔽),相等则报错。
func (c *Catalog) AddSource(ctx context.Context, src Source, required bool, priority int) error {
	caps, err := src.Sync(ctx)
	if err != nil {
		if required {
			return fmt.Errorf("catalog: required source %q sync failed: %w", src.Name(), err)
		}
		c.logger.Warn("optional source unavailable, skipped",
			slog.String("source", src.Name()), slog.String("err", err.Error()))
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cp := range caps {
		meta := cp.Meta()
		if meta.Risk > c.maxRisk {
			c.logger.Warn("capability rejected by admission (risk too high)",
				slog.String("ref", meta.Ref.String()), slog.String("risk", meta.Risk.String()))
			continue
		}
		key := meta.Ref.Key()
		if old, ok := c.entries[key]; ok {
			switch {
			case priority > old.priority:
				c.logger.Info("capability shadowed",
					slog.String("ref", meta.Ref.String()),
					slog.String("winner", src.Name()), slog.String("loser", old.source))
			case priority < old.priority:
				continue
			default:
				return fmt.Errorf("catalog: capability %s supplied by both %q and %q with equal priority",
					meta.Ref, old.source, src.Name())
			}
		}
		c.entries[key] = entry{cap: cp, source: src.Name(), priority: priority}
	}
	return nil
}

// Add 直接纳入代码侧构造的能力(等价于挂一个 Static source)。
func (c *Catalog) Add(caps ...capability.Capability) error {
	return c.AddSource(context.Background(), Static("local", caps...), true, 0)
}

// Get 按完整 ref 精确取一个能力。
func (c *Catalog) Get(refStr string) (capability.Capability, error) {
	ref, err := capability.ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[ref.Key()]
	if !ok {
		return nil, fmt.Errorf("catalog: capability %s not found", refStr)
	}
	meta := e.cap.Meta()
	if ref.Version != "" && meta.Ref.Version != ref.Version {
		return nil, fmt.Errorf("catalog: capability %s found but version is %q, want %q",
			ref.Key(), meta.Ref.Version, ref.Version)
	}
	return e.cap, nil
}

// Select 按 include/exclude 模式选品,exclude 优先。
// 返回的能力已完成模型可见短名的分配:默认 Ref.Name,
// 撞名升级为 <namespace>_<name>,结果按短名排序保证稳定。
func (c *Catalog) Select(include, exclude []string) ([]capability.Capability, error) {
	incPats, err := parsePatterns(include)
	if err != nil {
		return nil, err
	}
	excPats, err := parsePatterns(exclude)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	var picked []capability.Capability
	for _, e := range c.entries {
		ref := e.cap.Meta().Ref
		if !matchAny(ref, incPats) || matchAny(ref, excPats) {
			continue
		}
		picked = append(picked, e.cap)
	}
	c.mu.RUnlock()

	return assignToolNames(picked), nil
}

// SelectAll 选中目录里全部能力(exclude 可屏蔽)。「挂载全部」是显式
// 操作,不走 kind 通配匹配——通配不变式要求 kind 段精确。
func (c *Catalog) SelectAll(exclude []string) ([]capability.Capability, error) {
	excPats, err := parsePatterns(exclude)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	var picked []capability.Capability
	for _, e := range c.entries {
		if matchAny(e.cap.Meta().Ref, excPats) {
			continue
		}
		picked = append(picked, e.cap)
	}
	c.mu.RUnlock()
	return assignToolNames(picked), nil
}

// List 返回目录全量清单(排序稳定),供巡检与调试。
func (c *Catalog) List() []capability.Meta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	metas := make([]capability.Meta, 0, len(c.entries))
	for _, e := range c.entries {
		metas = append(metas, e.cap.Meta())
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Ref.Key() < metas[j].Ref.Key() })
	return metas
}

func parsePatterns(pats []string) ([]capability.Ref, error) {
	out := make([]capability.Ref, 0, len(pats))
	for _, p := range pats {
		r, err := capability.ParseRef(p)
		if err != nil {
			return nil, fmt.Errorf("catalog: bad pattern %q: %w", p, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func matchAny(ref capability.Ref, pats []capability.Ref) bool {
	for _, p := range pats {
		if ref.Match(p) {
			return true
		}
	}
	return false
}

// assignToolNames 分配模型可见短名:默认 Ref.Name;撞名的全部升级为
// namespace_name;仍撞(极端情况)再加 provider 前缀。
func assignToolNames(caps []capability.Capability) []capability.Capability {
	byName := map[string][]capability.Capability{}
	for _, cp := range caps {
		n := cp.Meta().Ref.Name
		byName[n] = append(byName[n], cp)
	}

	out := make([]capability.Capability, 0, len(caps))
	for name, group := range byName {
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		seen := map[string]bool{}
		for _, cp := range group {
			ref := cp.Meta().Ref
			alias := sanitize(ref.Domain + "_" + name)
			if seen[alias] {
				alias = sanitize(ref.Kind + "_" + ref.Domain + "_" + name)
			}
			seen[alias] = true
			out = append(out, capability.Rename(cp, alias))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Meta().Ref.Key() < out[j].Meta().Ref.Key()
	})
	return out
}

// sanitize 保证短名符合工具名字符集(字母数字下划线连字符)。
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}
