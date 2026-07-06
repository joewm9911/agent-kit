// pack.go:skillpack——外部 SKILL.md 技能包在框架内的执行形态(模型自主、
// 指令驱动;与确定性编排同为 kind=skill,消费方无差别选品,溯源看 Tags)。
//
// 渐进披露映射(docs/skillpack-design.md §2):
//
//	frontmatter name/description → capability.Meta(L1,常驻目录供选品)
//	Markdown 正文               → 内部 react 循环的 persona 层(L2,选中才加载)
//	打包文件                    → pack_read 只读工具(L3,按需;路径囚笼)
//	allowed-tools               → 工具面白名单(∩ 条目收紧 ∩ 目录)
//
// 执行是隔离子循环:正文不进宿主上下文,宿主只见 L1 与最终结果;Ring 0
// 闸门与内部 skill 同源(applyGates);条目声明 context: fork 时以调用方
// 对话快照起步(复用编排步骤的同名字段语义)。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
	"gopkg.in/yaml.v3"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/engine"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// PackManifest 是解析后的技能包。
type PackManifest struct {
	NS, Name     string
	Version      string
	Description  string
	Body         string   // SKILL.md 正文(L2)
	AllowedTools []string // frontmatter allowed-tools(CapRef 模式)
	Dir          string   // 本地物化目录
	Ref          string   // 来源 ref(观测/回溯)
	SHA          string   // 内容树哈希
	HasFiles     bool     // 除 SKILL.md 外还有文件(决定挂不挂 pack_read)
	// Runtimes 是包内脚本类型(按扩展名检测:.py→python .js/.mjs→node
	// .sh→bash),非空说明是脚本型技能包:装配层据此绑定 exec 工具
	// (工作目录=包目录),包风险经 exec 工具传播为 Dangerous。
	Runtimes []string
}

// PackOverrides 是 use: 条目的本地覆盖(名字覆盖走 PackSpec.Name,
// 物化目录与 lock 以最终名记账)。
type PackOverrides struct {
	Model    *ModelDecl // 专属模型,nil 跟随宿主默认
	MaxSteps int
	Tools    []string // 白名单收紧(与 allowed-tools 求交集)
	Context  string   // "" | fresh | fork(与编排步骤同义)
}

// LoadManifest 从已物化的包目录解析 manifest(pd 来自 EnsurePack)。
func LoadManifest(pd PackDir) (*PackManifest, error) {
	name, desc, allowed, body, err := parseSkillMD(filepath.Join(pd.Dir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("skillpack %s: %w", pd.Ref, err)
	}
	_ = name // 目录/最终名已由 EnsurePack 定(含覆盖);frontmatter name 仅作缺省来源
	hasFiles := false
	runtimeSet := map[string]bool{}
	_ = filepath.Walk(pd.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if rel, _ := filepath.Rel(pd.Dir, p); rel != "SKILL.md" {
			hasFiles = true
		}
		switch filepath.Ext(p) {
		case ".py":
			runtimeSet["python"] = true
		case ".js", ".mjs":
			runtimeSet["node"] = true
		case ".sh":
			runtimeSet["bash"] = true
		}
		return nil
	})
	var runtimes []string
	for r := range runtimeSet {
		runtimes = append(runtimes, r)
	}
	sort.Strings(runtimes)
	return &PackManifest{
		NS: pd.NS, Name: pd.Name, Version: pd.Version,
		Description: desc, Body: body, AllowedTools: allowed,
		Dir: pd.Dir, Ref: pd.Ref, SHA: pd.SHA, HasFiles: hasFiles,
		Runtimes: runtimes,
	}, nil
}

// parseSkillMD 解析 SKILL.md:YAML frontmatter(--- 包围)+ Markdown 正文。
// allowed-tools 兼容两种写法:YAML 列表,或空格/逗号分隔的字符串
// (Claude Code 生态两种都存在)。
func parseSkillMD(path string) (name, desc string, allowed []string, body string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, "", fmt.Errorf("读取 SKILL.md: %w", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", "", nil, "", fmt.Errorf("SKILL.md 缺 frontmatter(--- 开头)")
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", "", nil, "", fmt.Errorf("SKILL.md frontmatter 未闭合(缺结尾 ---)")
	}
	var fm struct {
		Name         string `yaml:"name"`
		Description  string `yaml:"description"`
		AllowedTools any    `yaml:"allowed-tools"`
	}
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return "", "", nil, "", fmt.Errorf("SKILL.md frontmatter 解析失败: %w", err)
	}
	if fm.Description == "" {
		return "", "", nil, "", fmt.Errorf("SKILL.md frontmatter 缺 description(L1 选品依据)")
	}
	switch v := fm.AllowedTools.(type) {
	case nil:
	case string:
		allowed = strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
	case []any:
		for _, it := range v {
			allowed = append(allowed, fmt.Sprint(it))
		}
	default:
		return "", "", nil, "", fmt.Errorf("SKILL.md allowed-tools: 期望列表或字符串")
	}
	return fm.Name, fm.Description, allowed, strings.TrimSpace(strings.TrimPrefix(body, "\n")), nil
}

// BuildPack 把技能包装配为能力:白名单选品 → Ring 0 闸门(与 Build 同源)
// → react 子循环 → capability。kind 与内部 skill 一致(=skill):对消费方
// (选品/审批规则)"skill 就是 skill",来源是属性不是类型——溯源在 Tags
// (ref:/sha:),风险治理靠 Risk 分级(脚本包 Dangerous)。
// extra 是装配层追加的工具(脚本型包的 exec 工具,工作目录已绑定包目录),
// 不经白名单过滤——它们是包自己的执行原语,不是宿主能力。
func BuildPack(ctx context.Context, m *PackManifest, ov PackOverrides, deps Deps, extra ...capability.Capability) (capability.Capability, error) {
	if ov.Context != "" && ov.Context != "fresh" && ov.Context != "fork" {
		return nil, fmt.Errorf("skillpack %s: context 只支持 fresh|fork,got %q", m.Ref, ov.Context)
	}

	// 模型:专属或跟随宿主默认(与 Build 同规则)。
	mdl := deps.DefaultModel
	if ov.Model != nil {
		built, err := buildDeclModel(ctx, ov.Model, deps.Retry)
		if err != nil {
			return nil, fmt.Errorf("skillpack %s: %w", m.Ref, err)
		}
		mdl = built
	}
	if mdl == nil {
		return nil, fmt.Errorf("skillpack %s: no model (declare model or provide default)", m.Ref)
	}

	// 工具白名单:allowed-tools ∩ 条目 tools(收紧)→ 目录选品。
	include := m.AllowedTools
	if len(ov.Tools) > 0 {
		include = intersectRefs(include, ov.Tools)
	}
	var caps []capability.Capability
	if len(include) > 0 {
		if deps.Catalog == nil {
			return nil, fmt.Errorf("skillpack %s: 声明了工具白名单但装配层未提供目录", m.Ref)
		}
		var err error
		if caps, err = deps.Catalog.Select(include, nil); err != nil {
			return nil, fmt.Errorf("skillpack %s: select tools: %w", m.Ref, err)
		}
		if err := checkExactRefs(include, caps); err != nil {
			return nil, fmt.Errorf("skillpack %s: %w", m.Ref, err)
		}
	}
	// L3 读取口:包里除 SKILL.md 还有文件才挂(工具面不承诺不存在的东西)。
	if m.HasFiles {
		caps = append(caps, packReadCap(m.Dir))
	}
	caps = append(caps, extra...)

	// 风险 = 白名单工具的最大风险(纯指令包无工具 → readonly)。
	risk := capability.RiskReadonly
	for _, c := range caps {
		if r := c.Meta().Risk; r > risk {
			risk = r
		}
	}

	caps = applyGates(caps, mdl, deps)

	layers := loop.PromptLayers{Loop: loop.DefaultLoopPromptNoTodo, Persona: m.Body}
	runner, err := engine.Build(ctx, "react", &engine.Assembly{
		Model:        mdl,
		Capabilities: caps,
		MaxSteps:     ov.MaxSteps,
		Modifier:     layers.Modifier(),
	})
	if err != nil {
		return nil, fmt.Errorf("skillpack %s: build engine: %w", m.Ref, err)
	}

	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "skill", Domain: m.NS, Name: m.Name, Version: m.Version},
		Description: m.Description,
		Params:      capability.SingleParam("input", "任务描述(自然语言)"),
		Risk:        risk,
		Tags:        []string{"ref:" + m.Ref, "sha:" + m.SHA[:12]},
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		task := capability.ParseSingle(argsJSON, "input")
		// 上下文边界与内部 skill 一致:独立子循环,过程不回流宿主;
		// context: fork 时以调用方对话快照 + 任务起步。
		if ov.Context == "fork" {
			ctx = runctx.WithForkContext(ctx)
		}
		out, err := runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}), nil
}

// packReadCap 是包目录的只读工具(L3 渐进披露):list 列文件、read 读内容。
// 路径囚笼:解析后的绝对路径必须仍在包目录内,拒绝 ../ 与绝对路径逃逸。
func packReadCap(dir string) capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "pack", Name: "pack_read"},
		Description: "读取本技能包内的打包文件。path 为空列出全部文件;非空返回该文件内容(只读,仅限包内)。",
		Params:      capability.SingleParam("path", "包内相对路径;空 = 列出文件清单"),
	}
	return capability.New(meta, func(_ context.Context, argsJSON string) (string, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &args) // path 可选:缺省列清单
		rel := args.Path
		if rel == "" {
			var files []string
			_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				r, _ := filepath.Rel(dir, p)
				files = append(files, filepath.ToSlash(r))
				return nil
			})
			return strings.Join(files, "\n"), nil
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		abs, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}
		base, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return "路径越界:只能读取技能包内的文件。", nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Sprintf("读取失败:%v", err), nil
		}
		return string(data), nil
	})
}

// intersectRefs 取两个 CapRef 模式列表的交集(精确字符串匹配;收紧语义:
// 条目 tools 里不在 allowed-tools 中的引用直接丢弃)。allowed 为空时
// 视为"包未声明白名单",以条目 tools 为准。
func intersectRefs(allowed, tighten []string) []string {
	if len(allowed) == 0 {
		return tighten
	}
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	var out []string
	for _, t := range tighten {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}
