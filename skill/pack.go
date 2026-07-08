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
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

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
	// 以下来自 frontmatter,与 eino ADK Skill middleware 协议对齐:
	// Context 已归一为全库统一词表:""/fresh(隔离,默认)| fork(带
	// 调用方对话快照)。第三方包的旧值在解析处兼容映射(fork→fresh、
	// fork_with_context→fork)并记 warn。Agent/Model 按名指定执行
	// agent 或模型,经 Deps.AgentHub/ModelHub 解析。
	Context string
	Agent   string
	Model   string
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
	name, desc, allowed, front, body, err := parseSkillMD(filepath.Join(pd.Dir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("skillpack %s: %w", pd.Ref, err)
	}
	_ = name // 目录/最终名已由 EnsurePack 定(含覆盖);frontmatter name 仅作缺省来源
	// context 词表归一:统一为 fresh(隔离)| fork(快照)。eino 协议的
	// 旧值是第三方包文件,保留兼容映射(fork=隔离→fresh、
	// fork_with_context=快照→fork)并 warn;本地配置无兼容负担。
	switch front.Context {
	case "", "fresh":
		front.Context = "fresh"
	case "fork_with_context":
		slog.Warn("skillpack frontmatter context 旧值已映射", slog.String("pack", pd.Ref),
			slog.String("old", "fork_with_context"), slog.String("new", "fork(带快照)"))
		front.Context = "fork"
	case "fork":
		// 歧义值:eino 旧语义=隔离,新词表=快照——按包文件的原语义映射为
		// fresh,避免第三方包行为静默改变。
		slog.Warn("skillpack frontmatter context 旧值已映射", slog.String("pack", pd.Ref),
			slog.String("old", "fork(eino 语义=隔离)"), slog.String("new", "fresh(隔离)"))
		front.Context = "fresh"
	default:
		return nil, fmt.Errorf("skillpack %s: frontmatter context only supports fresh|fork (legacy values fork/fork_with_context are auto-mapped), got %q", pd.Ref, front.Context)
	}
	if front.Agent != "" && front.Model != "" {
		return nil, fmt.Errorf("skillpack %s: frontmatter agent and model are mutually exclusive (in agent mode the model is decided by that agent itself)", pd.Ref)
	}
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
		Context:  front.Context, Agent: front.Agent, Model: front.Model,
	}, nil
}

// skillFront 是 frontmatter 里与执行语义相关的扩展字段(eino/agentskills
// 协议对齐)。
type skillFront struct {
	Context string // "" | fork | fork_with_context
	Agent   string // 按名指定执行 agent(经 Deps.AgentHub)
	Model   string // 按名指定模型(经 Deps.ModelHub)
}

// parseSkillMD 解析 SKILL.md:YAML frontmatter(--- 包围)+ Markdown 正文。
// allowed-tools 兼容两种写法:YAML 列表,或空格/逗号分隔的字符串
// (Claude Code 生态两种都存在)。
func parseSkillMD(path string) (name, desc string, allowed []string, front2 skillFront, body string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, front2, "", fmt.Errorf("read SKILL.md: %w", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", "", nil, front2, "", fmt.Errorf("SKILL.md is missing frontmatter (must start with ---)")
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", "", nil, front2, "", fmt.Errorf("SKILL.md frontmatter is not closed (missing trailing ---)")
	}
	var fm struct {
		Name         string `yaml:"name"`
		Description  string `yaml:"description"`
		AllowedTools any    `yaml:"allowed-tools"`
		// 以下三个字段与 eino ADK Skill middleware / agentskills.io 协议对齐:
		// context 取 fork(隔离)| fork_with_context(带调用方对话快照);
		// agent/model 按名指定执行 agent 或模型(经装配层 Hub 解析)。
		Context string `yaml:"context"`
		Agent   string `yaml:"agent"`
		Model   string `yaml:"model"`
	}
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return "", "", nil, front2, "", fmt.Errorf("SKILL.md frontmatter parse failed: %w", err)
	}
	if fm.Description == "" {
		return "", "", nil, front2, "", fmt.Errorf("SKILL.md frontmatter is missing description (the L1 selection basis)")
	}
	front2 = skillFront{Context: fm.Context, Agent: fm.Agent, Model: fm.Model}
	switch v := fm.AllowedTools.(type) {
	case nil:
	case string:
		allowed = strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
	case []any:
		for _, it := range v {
			allowed = append(allowed, fmt.Sprint(it))
		}
	default:
		return "", "", nil, front2, "", fmt.Errorf("SKILL.md allowed-tools: expected a list or a string")
	}
	return fm.Name, fm.Description, allowed, front2, strings.TrimSpace(strings.TrimPrefix(body, "\n")), nil
}

// BuildPack 把技能包装配为能力:白名单选品 → Ring 0 闸门(与 Build 同源)
// → react 子循环 → capability。kind 与内部 skill 一致(=skill):对消费方
// (选品/审批规则)"skill 就是 skill",来源是属性不是类型——溯源在 Tags
// (ref:/sha:),风险治理靠 Risk 分级(脚本包 Dangerous)。
// extra 是装配层追加的工具(脚本型包的 exec 工具,工作目录已绑定包目录),
// 不经白名单过滤——它们是包自己的执行原语,不是宿主能力。
// packSeq 给 pack 调用的执行域段编号(与 skill.Build 的调用序同规范)。
var packSeq atomic.Int64

func BuildPack(ctx context.Context, m *PackManifest, ov PackOverrides, deps Deps, extra ...capability.Capability) (capability.Capability, error) {
	if ov.Context != "" && ov.Context != "fresh" && ov.Context != "fork" {
		return nil, fmt.Errorf("skillpack %s: context only supports fresh|fork, got %q", m.Ref, ov.Context)
	}
	// 快照 fork 判定:本地覆盖(agent-kit 语义,fork=快照)优先;否则
	// frontmatter 公共协议语义——fork_with_context=快照,fork=隔离(即默认
	// 子循环,无需动作)。
	snapshotFork := ov.Context == "fork" || (ov.Context == "" && m.Context == "fork") // 词表已归一:fork = 带快照

	// frontmatter agent: 委托执行——技能内容交给指定的已装配 agent,
	// 工具面/治理/模型都是该 agent 自己的,本包不再建子循环。
	if m.Agent != "" {
		if deps.AgentHub == nil {
			return nil, fmt.Errorf("skillpack %s: frontmatter declares agent: %q but the assembly environment provides no AgentHub", m.Ref, m.Agent)
		}
		return buildAgentDelegate(m, snapshotFork, deps.AgentHub), nil
	}

	// 模型:条目覆盖 > frontmatter model:(经 ModelHub)> 宿主默认。
	mdl := deps.DefaultModel
	if ov.Model != nil {
		built, err := buildDeclModel(ctx, ov.Model, deps.Retry)
		if err != nil {
			return nil, fmt.Errorf("skillpack %s: %w", m.Ref, err)
		}
		mdl = built
	} else if m.Model != "" {
		if deps.ModelHub == nil {
			return nil, fmt.Errorf("skillpack %s: frontmatter declares model: %q but the assembly environment provides no ModelHub (app-level models: named models)", m.Ref, m.Model)
		}
		built, err := deps.ModelHub(ctx, m.Model)
		if err != nil {
			return nil, fmt.Errorf("skillpack %s: model %q: %w", m.Ref, m.Model, err)
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
			return nil, fmt.Errorf("skillpack %s: a tool allowlist is declared but the assembly layer provided no catalog", m.Ref)
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
		caps = append(caps, packReadCap(packFS(m.Dir)))
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
		Model: loop.ReviewModel(mdl, loop.RepeatBreakReviewer(), loop.FinishReviewer(),
			loop.CheckedReviewer(loop.DeniedCallsCheck)), // 统一评审循环(子循环同套纪律)
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
		Params:      capability.SingleParam("input", "Task description (natural language)"),
		Risk:        risk,
		Tags:        []string{"ref:" + m.Ref, "sha:" + m.SHA[:12]},
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		task := capability.ParseSingle(argsJSON, "input")
		// 执行域压栈:pack 内部步骤在进度/todo 等按域隔离的机制里
		// 不与宿主混淆(与 skill.Build 的 comp: 段同规范)。
		ctx = runctx.WithScopePush(ctx, fmt.Sprintf("comp:%s#%d", m.Name, packSeq.Add(1)))
		// 上下文边界与内部 skill 一致:独立子循环,过程不回流宿主;
		// 快照 fork 时以调用方对话快照 + 任务起步。
		if snapshotFork {
			ctx = runctx.WithForkContext(ctx)
		}
		out, err := runner.Generate(ctx, loop.ForkMessages(ctx, schema.UserMessage(task)))
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}), nil
}

// buildAgentDelegate 构造 frontmatter agent: 模式的能力:调用期经 AgentHub
// 解析目标 agent(agent 装配晚于技能,名字合法性由装配层在装配期校验),
// 把 L2 正文 + 用户任务组成完整指令交其执行。风险标 mutating(实际风险
// 由目标 agent 的工具面与其审批/预算治理决定,这里取保守中档)。
func buildAgentDelegate(m *PackManifest, snapshotFork bool, hub func(string) (capability.Capability, bool)) capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "skill", Domain: m.NS, Name: m.Name, Version: m.Version},
		Description: m.Description,
		Params:      capability.SingleParam("input", "Task description (natural language)"),
		Risk:        capability.RiskMutating,
		Tags:        []string{"ref:" + m.Ref, "sha:" + m.SHA[:12], "agent:" + m.Agent},
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		target, ok := hub(m.Agent)
		if !ok {
			return "", fmt.Errorf("skillpack %s: the agent %q specified in frontmatter does not exist", m.Ref, m.Agent)
		}
		if snapshotFork {
			ctx = runctx.WithForkContext(ctx)
		}
		task := capability.ParseSingle(argsJSON, "input")
		instr := "[Skill instructions]\n" + m.Body + "\n\n[Task]\n" + task
		payload, _ := json.Marshal(map[string]string{"task": instr})
		return capability.Invoke(ctx, target, string(payload))
	})
}

// packFS 把一个已安装的包目录包成 fs.FS(pack_read 的承载)。这是接入点:
// bundled 包(随配置内嵌)可改为 fs.Sub(资源FS, 包目录),pack_read 代码不变。
func packFS(dir string) fs.FS { return os.DirFS(dir) }

// packReadCap 是包内容的只读工具(L3 渐进披露,即 fs cap):list 列文件、
// read 读内容。以 fs.FS 承载包根,内嵌/本地/远程一套代码;fs.FS 的
// ValidPath 语义天然拒绝 '..' 与绝对路径逃逸,免去手写路径囚笼。
func packReadCap(packFS fs.FS) capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: "pack", Name: "pack_read"},
		Description: "Read a reference file bundled with the skill pack (limited to docs/templates packaged inside the pack; an empty path lists the manifest). Note: the user's files are not inside the pack — to read or write the user's files, use a script execution tool (e.g. python).",
		Params:      capability.SingleParam("path", "Relative path inside the pack; empty = list the file manifest"),
	}
	return capability.New(meta, func(_ context.Context, argsJSON string) (string, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &args) // path 可选:缺省列清单
		if args.Path == "" {
			var files []string
			_ = fs.WalkDir(packFS, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				files = append(files, p)
				return nil
			})
			return strings.Join(files, "\n"), nil
		}
		clean := path.Clean(args.Path)
		if !fs.ValidPath(clean) { // '..'、绝对路径、逃出包根:囚笼由 fs.FS 强制
			return "path out of bounds: pack_read can only read files bundled with the skill pack. to access the user's file paths, use a script execution tool instead (e.g. python's open()).", nil
		}
		data, err := fs.ReadFile(packFS, clean)
		if err != nil {
			return fmt.Sprintf("read failed: %v", err), nil
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
