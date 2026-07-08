// skillpack.go:外部 skillpack(use: 链接)的装配接线。启动期下载安装
// (v1 主路径,见 docs/skillpack-design.md):EnsurePack 物化到 .skills +
// skills.lock 校验 → LoadManifest → skill.BuildPack → 进目录。
// 打包期 CLI(sync/verify)延后,核心逻辑已收口在 skill 包与本文件,
// 届时只需薄壳。
package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/skill"
)

// SkillpacksConfig 是 app 级外部技能包策略。
type SkillpacksConfig struct {
	// Dir 覆盖安装目录(相对值以 work_dir 为基准)。默认固定约定:
	// <work_dir>/agent-kit/.skills——agent-kit 是 SDK,落盘产物收口在
	// 宿主项目的 agent-kit/ 命名空间下(对齐 node_modules/.terraform 心智)。
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
		return skill.PackOptions{}, fmt.Errorf("skillpacks.sync: only auto|require-local are supported, got %q", sc.Sync)
	}
}

// root 解析安装目录:默认 <state_dir>/agent-kit/.skills(固定约定);
// dir 显式覆盖时,相对值同样以 state_dir 为基准。
func (sc SkillpacksConfig) root(stateDir string) string {
	base := resolveStateDir(stateDir)
	if sc.Dir == "" {
		return filepath.Join(base, "agent-kit", ".skills")
	}
	if filepath.IsAbs(sc.Dir) {
		return sc.Dir
	}
	return filepath.Join(base, sc.Dir)
}

// resolveStateDir 解析可写状态目录。默认链:显式 state_dir → 环境
// AGENTKIT_STATE_DIR → $XDG_STATE_HOME/agentkit → ~/.local/state/agentkit。
// 相对值以 cwd 解析为绝对。
func resolveStateDir(stateDir string) string {
	if stateDir == "" {
		stateDir = os.Getenv("AGENTKIT_STATE_DIR")
	}
	if stateDir == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			stateDir = filepath.Join(xdg, "agentkit")
		} else if home, err := os.UserHomeDir(); err == nil {
			stateDir = filepath.Join(home, ".local", "state", "agentkit")
		} else {
			stateDir = "."
		}
	}
	if abs, err := filepath.Abs(stateDir); err == nil {
		return abs
	}
	return stateDir
}

// rejectWorkDir 拦截已拆义的旧键 work_dir(fail fast 即迁移指南)。
func rejectWorkDir(legacy *string, where string) error {
	if legacy != nil {
		return fmt.Errorf("%s: work_dir has been split — read-only resources (config/prompts/skill packs) are carried by the resource loader (local dir / embed.FS), and writable runtime state moves under state_dir (skill installs, file backends, trajectory). Set state_dir instead", where)
	}
	return nil
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
	spec skill.PackSpec, ov skill.PackOverrides, deps skill.Deps, execCfg ExecConfig,
	hubs *skillHubs) (capability.Capability, error) {

	pd, err := skill.EnsurePack(ctx, root, spec, opts)
	if err != nil {
		return nil, err
	}
	m, err := skill.LoadManifest(pd)
	if err != nil {
		return nil, err
	}
	// frontmatter agent:/model: 的按名解析环境(eino AgentHub/ModelHub 的
	// 本地等价物)。agent 名在装配期对照"已声明 agent"校验,fail fast;
	// 实例查找延迟到调用期(agent 装配晚于技能)。
	if hubs != nil {
		deps.AgentHub = hubs.agents.lookup
		deps.ModelHub = hubs.models
		if m.Agent != "" && !hubs.known[m.Agent] {
			return nil, fmt.Errorf("skillpack %s: agent %q specified in frontmatter is not declared in this app", spec.Use, m.Agent)
		}
	} else if m.Agent != "" {
		return nil, fmt.Errorf("skillpack %s: frontmatter declares agent: %q, but the current assembly path does not support referencing agents by name", spec.Use, m.Agent)
	}
	var extra []capability.Capability
	if len(m.Runtimes) > 0 {
		tools := make([]map[string]any, 0, len(m.Runtimes))
		for _, rt := range m.Runtimes {
			tools = append(tools, map[string]any{"name": rt, "runtime": rt})
		}
		// app 级默认沙箱策略透传:pack 脚本随 exec.default_sandbox 进沙箱
		// (require_sandbox 时无沙箱即 fail fast),workdir 仍绑包目录。
		execConf := execCfg.injectInto(map[string]any{
			"workdir": m.Dir, "timeout": "60s", "tools": tools,
		})
		src, err := source.New(ctx, "exec", "pack", execConf)
		if err != nil {
			return nil, fmt.Errorf("skillpack %s contains scripts (%v) and needs an exec source (blank-import impl/source/exectool): %w", spec.Use, m.Runtimes, err)
		}
		if extra, err = src.Sync(ctx); err != nil {
			return nil, fmt.Errorf("skillpack %s: exec tools: %w", spec.Use, err)
		}
	}
	return skill.BuildPack(ctx, m, ov, deps, extra...)
}
