// skillpack.go:外部 skillpack(use: 链接)的装配接线。启动期下载安装
// (v1 主路径,见 docs/skillpack-design.md):EnsurePack 物化到 .skills +
// skills.lock 校验 → LoadManifest → skill.BuildPack → 进目录。
// 打包期 CLI(sync/verify)延后,核心逻辑已收口在 skill 包与本文件,
// 届时只需薄壳。
package config

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/skill"
)

// SkillpacksConfig 是 app 级外部技能包策略。
type SkillpacksConfig struct {
	// Dir 是物化目录,默认 .skills(相对配置文件所在目录)。
	Dir string `yaml:"dir"`
	// Sync:auto(默认,缺失即下载)| require-local(缺失 fail fast,
	// 为打包期物化预留的收紧档)。
	Sync string `yaml:"sync"`
	// AllowUnpinned 放行未锁定版本的 ref(lock 仍会锁死首次解析结果)。
	AllowUnpinned bool `yaml:"allow_unpinned"`
}

func (sc SkillpacksConfig) options() (skill.PackOptions, error) {
	switch sc.Sync {
	case "", "auto":
		return skill.PackOptions{AllowUnpinned: sc.AllowUnpinned}, nil
	case "require-local":
		return skill.PackOptions{RequireLocal: true, AllowUnpinned: sc.AllowUnpinned}, nil
	default:
		return skill.PackOptions{}, fmt.Errorf("skillpacks.sync: 只支持 auto|require-local,got %q", sc.Sync)
	}
}

// root 解析物化目录:相对路径以配置文件所在目录为基准。
func (sc SkillpacksConfig) root(baseDir string) string {
	dir := sc.Dir
	if dir == "" {
		dir = ".skills"
	}
	if !filepath.IsAbs(dir) && baseDir != "" {
		dir = filepath.Join(baseDir, dir)
	}
	return dir
}

// isExternalRef 判定 use: 值是否外部链接(与内部 component 引用值域天然
// 不重叠:内部是 "components/..." 或 cap:// 形态)。
func isExternalRef(use string) bool {
	return strings.HasPrefix(use, "github.com/") ||
		strings.HasPrefix(use, "https://") ||
		strings.HasPrefix(use, "http://127.0.0.1") ||
		strings.HasPrefix(use, "http://localhost") ||
		strings.HasPrefix(use, "file:")
}

// buildSkillpack 走完一条外部引用的全链路:物化 → 解析 → 组合。
// 脚本型包(检测到 runtimes)经 source 注册表构造 exec 工具,工作目录
// 绑定包目录——config 不碰 impl,exectool 未空导入时 fail fast 指路。
func buildSkillpack(ctx context.Context, root string, opts skill.PackOptions,
	spec skill.PackSpec, ov skill.PackOverrides, deps skill.Deps) (capability.Capability, error) {

	pd, err := skill.EnsurePack(ctx, root, spec, opts)
	if err != nil {
		return nil, err
	}
	m, err := skill.LoadManifest(pd)
	if err != nil {
		return nil, err
	}
	var extra []capability.Capability
	if len(m.Runtimes) > 0 {
		tools := make([]map[string]any, 0, len(m.Runtimes))
		for _, rt := range m.Runtimes {
			tools = append(tools, map[string]any{"name": rt, "runtime": rt})
		}
		src, err := source.New(ctx, "exec", "pack", map[string]any{
			"workdir": m.Dir, "timeout": "60s", "tools": tools,
		})
		if err != nil {
			return nil, fmt.Errorf("skillpack %s 含脚本(%v),需要 exec 源(空导入 impl/source/exectool): %w", spec.Use, m.Runtimes, err)
		}
		if extra, err = src.Sync(ctx); err != nil {
			return nil, fmt.Errorf("skillpack %s: exec tools: %w", spec.Use, err)
		}
	}
	return skill.BuildPack(ctx, m, ov, deps, extra...)
}
