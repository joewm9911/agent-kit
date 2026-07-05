// graph.go 是编排族引擎(graph / workflow)的执行器,与循环族引擎
// (react/direct/…)同处 engine 包,和 `engine:` 配置面一一对应。
//
// 声明是带 needs 的步骤列表,语义是 DAG——不写 needs 即依赖上一步
// (退化为串行链),显式 needs 表达并行与汇合。所有引用在装配期解析
// 锁定并完成校验(fail fast),运行期没有大脑做路由,执行路径是强保证的。
//
// 执行器自持(不经 compose.Graph):步骤级超时/重试、并发状态合并、
// 以及后续挂起/恢复所需的可序列化执行位置,都要求对执行过程的完全
// 控制。产物仍是 capability.Capability,双形态挂载不受影响。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
)

// Step 是编排图上的一个节点。use 只做引用(能力声明与能力使用分离):
//
//	components/<name>        本命名空间的执行单元
//	tools/<source>/<name>    本命名空间的工具
//	model                    单次模型调用(args 渲染为提示词)
//	cap://skill...           其他命名空间的 skill(跨 ns 的唯一接口)
type Step struct {
	Name string `yaml:"name"`
	Use  string `yaml:"use"`
	// Args 是入参模板,可引用 {参数名} 与 {步骤名}(依赖步骤的输出)。
	// 为空时透传 skill 的原始入参 JSON。
	Args string `yaml:"args"`
	// Needs 是依赖的步骤名,缺省为上一声明步骤(首步缺省无依赖)。
	Needs []string `yaml:"needs"`
	// Timeout 是本步骤单次执行的超时,超时视为步骤失败(中断整图)。
	Timeout capability.Duration `yaml:"timeout"`
	// Retry 是失败后的重试次数(总尝试 = Retry+1),重试间做短退避。
	Retry int `yaml:"retry"`
	// Context 声明目标能力的上下文起点:空/fresh(默认,从零起步)
	// | fork(以最外层 agent 的对话快照 + 任务起步;背景无损继承,
	// 隔离方向不变,只对带内部循环的能力有意义)。
	Context string `yaml:"context"`
}

// GraphDeclaration 是新形态 skill 的完整声明:对外接口(Description
// + Params)加编排(Steps)。执行单元与工具的声明在命名空间层,这里
// 只出现引用。
type GraphDeclaration struct {
	// Kind 是产物的 cap kind:空=skill;component 装配时置 "component"。
	Kind        string                          `yaml:"-"`
	Name        string                          `yaml:"name"`
	Version     string                          `yaml:"version"`
	Description string                          `yaml:"description"`
	Params      map[string]capability.ParamDecl `yaml:"params"`
	Steps       []Step                          `yaml:"steps"`
	// Output 是产出步骤名,缺省为最后一个声明的步骤。
	Output string `yaml:"output"`
}

// StepResolver 在装配期把 use 引用解析为能力。由命名空间装配层提供,
// 边界规则(工具不出 ns、跨 ns 只能引 skill)在解析器里落实。
type StepResolver func(use string) (capability.Capability, error)

// compiledStep 是装配校验后的节点:能力已解析锁定,依赖已编号。
type compiledStep struct {
	step       Step
	cap        capability.Capability
	needs      []int
	dependents []int
}

type graphPlan struct {
	steps  []compiledStep
	params map[string]capability.ParamDecl
	output int // 产出步骤下标
}

// BuildGraph 把声明编译为 skill 能力:校验结构 → 解析全部引用 →
// 计算依赖图 → 包装为 capability。ns 是所属命名空间名。
func BuildGraph(_ context.Context, decl *GraphDeclaration, ns string, resolve StepResolver) (capability.Capability, error) {
	if decl.Name == "" || len(decl.Steps) == 0 {
		return nil, fmt.Errorf("skill: name and steps are required")
	}
	plan, err := compileGraph(decl, resolve)
	if err != nil {
		return nil, fmt.Errorf("skill %s/%s: %w", ns, decl.Name, err)
	}

	// 风险传播:skill 的有效风险 = 各步骤能力风险的最大值
	risk := capability.RiskReadonly
	for _, s := range plan.steps {
		if s.cap != nil {
			if r := s.cap.Meta().Risk; r > risk {
				risk = r
			}
		}
	}

	kind := decl.Kind
	if kind == "" {
		kind = "skill"
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: kind, Domain: ns, Name: decl.Name, Version: decl.Version},
		Description: decl.Description,
		Params:      capability.ParamsSchema(decl.Params),
		Risk:        risk,
	}
	return capability.New(meta, plan.run), nil
}

func compileGraph(decl *GraphDeclaration, resolve StepResolver) (*graphPlan, error) {
	for name := range decl.Params {
		if strings.HasPrefix(name, "$") {
			return nil, fmt.Errorf("param %q: names must not start with $ (reserved for builtin variables)", name)
		}
	}

	n := len(decl.Steps)
	index := make(map[string]int, n)
	for i, s := range decl.Steps {
		if s.Name == "" || s.Use == "" {
			return nil, fmt.Errorf("step %d: name and use are required", i)
		}
		if strings.HasPrefix(s.Name, "$") {
			return nil, fmt.Errorf("step %q: names must not start with $ (reserved for builtin variables)", s.Name)
		}
		if _, dup := index[s.Name]; dup {
			return nil, fmt.Errorf("duplicate step name %q", s.Name)
		}
		if _, clash := decl.Params[s.Name]; clash {
			return nil, fmt.Errorf("step %q collides with a param name (template refs would be ambiguous)", s.Name)
		}
		index[s.Name] = i
	}

	steps := make([]compiledStep, n)
	for i, s := range decl.Steps {
		if s.Context != "" && s.Context != "fresh" && s.Context != "fork" {
			return nil, fmt.Errorf("step %q: bad context %q (want fresh|fork)", s.Name, s.Context)
		}
		cs := compiledStep{step: s}
		// 缺省依赖:上一声明步骤;显式 needs 覆盖
		if s.Needs == nil && i > 0 {
			cs.needs = []int{i - 1}
		}
		for _, need := range s.Needs {
			j, ok := index[need]
			if !ok {
				return nil, fmt.Errorf("step %q needs unknown step %q", s.Name, need)
			}
			if j == i {
				return nil, fmt.Errorf("step %q depends on itself", s.Name)
			}
			cs.needs = append(cs.needs, j)
		}
		c, err := resolve(s.Use)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", s.Name, err)
		}
		cs.cap = c
		steps[i] = cs
	}
	for i := range steps {
		for _, j := range steps[i].needs {
			steps[j].dependents = append(steps[j].dependents, i)
		}
	}

	if err := checkAcyclic(steps); err != nil {
		return nil, err
	}
	if err := checkTemplateRefs(decl, steps, index); err != nil {
		return nil, err
	}

	output := n - 1
	if decl.Output != "" {
		j, ok := index[decl.Output]
		if !ok {
			return nil, fmt.Errorf("output refers to unknown step %q", decl.Output)
		}
		output = j
	}
	return &graphPlan{steps: steps, params: decl.Params, output: output}, nil
}

// checkAcyclic 用 Kahn 拓扑排序检测环。
func checkAcyclic(steps []compiledStep) error {
	n := len(steps)
	indeg := make([]int, n)
	for i := range steps {
		indeg[i] = len(steps[i].needs)
	}
	queue := make([]int, 0, n)
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	visited := 0
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		visited++
		for _, j := range steps[i].dependents {
			if indeg[j]--; indeg[j] == 0 {
				queue = append(queue, j)
			}
		}
	}
	if visited != n {
		var cyc []string
		for i, d := range indeg {
			if d > 0 {
				cyc = append(cyc, steps[i].step.Name)
			}
		}
		sort.Strings(cyc)
		return fmt.Errorf("dependency cycle among steps: %s", strings.Join(cyc, ", "))
	}
	return nil
}

var tplRef = regexp.MustCompile(`\{(\$?[\p{L}\p{N}_\-]+)\}`)

// builtinVars 是 $ 前缀的保留变量:由框架在运行时直接注入,不经过
// 主脑转写。$input = 用户本轮输入原文(runctx.Input),穿透任意嵌套
// 深度。注意:$input 是不可信文本,把它引进哪个步骤等于在那里显式
// 开一个注入面——显式声明的价值正在于开了口子的地方看得见。
var builtinVars = map[string]bool{
	"$input": true,
}

// checkTemplateRefs 校验数据流与控制流一致:args 模板引用的每个占位
// 要么是参数,要么是 needs 传递闭包内的步骤——并行分支下引用闭包外
// 步骤的输出是竞态,直接拒绝装配;未知占位视为笔误同样拒绝。
func checkTemplateRefs(decl *GraphDeclaration, steps []compiledStep, index map[string]int) error {
	closures := make([]map[int]bool, len(steps))
	var closure func(i int) map[int]bool
	closure = func(i int) map[int]bool {
		if closures[i] != nil {
			return closures[i]
		}
		set := map[int]bool{}
		closures[i] = set // 先占位;无环已由 checkAcyclic 保证
		for _, j := range steps[i].needs {
			set[j] = true
			for k := range closure(j) {
				set[k] = true
			}
		}
		return set
	}

	for i, s := range steps {
		for _, m := range tplRef.FindAllStringSubmatch(s.step.Args, -1) {
			ref := m[1]
			if strings.HasPrefix(ref, "$") {
				if !builtinVars[ref] {
					return fmt.Errorf("step %q args references unknown builtin variable {%s}", s.step.Name, ref)
				}
				continue
			}
			if _, isParam := decl.Params[ref]; isParam {
				continue
			}
			j, isStep := index[ref]
			if !isStep {
				return fmt.Errorf("step %q args references unknown placeholder {%s} (not a param or step)", s.step.Name, ref)
			}
			if !closure(i)[j] {
				return fmt.Errorf("step %q args references {%s} which is not in its needs closure (add it to needs)", s.step.Name, ref)
			}
		}
	}
	return nil
}

// run 执行编排图:每次调用一份独立 state(vars),按依赖就绪并行推进,
// 任一步骤失败取消其余分支并中断整图。
func (p *graphPlan) run(ctx context.Context, argsJSON string) (string, error) {
	vars := map[string]string{}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
		for k, v := range args {
			vars[k] = fmt.Sprint(v)
		}
	}
	// 保留变量最后注入:框架直取,调用方传入的同名键不能顶掉它。
	vars["$input"] = runctx.Input(ctx)
	var missing []string
	for name, d := range p.params {
		if _, ok := vars[name]; !ok && d.Required {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		// 以结果回传而非错误:让上级大脑补齐参数重试,循环不中断。
		return fmt.Sprintf("调用未执行:缺少必填参数 %s。", strings.Join(missing, ", ")), nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	n := len(p.steps)
	indeg := make([]int, n)
	for i := range p.steps {
		indeg[i] = len(p.steps[i].needs)
	}

	var (
		mu       sync.Mutex // 保护 vars 与 indeg
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	var exec func(i int)
	exec = func(i int) {
		defer wg.Done()
		if ctx.Err() != nil {
			return
		}
		s := &p.steps[i]
		mu.Lock()
		args := renderVars(s.step.Args, vars)
		mu.Unlock()
		if s.step.Args == "" {
			args = argsJSON // 空模板透传原始入参(passthrough 场景)
		}
		out, err := runStep(ctx, s, args)
		if err != nil {
			errOnce.Do(func() {
				firstErr = fmt.Errorf("step %s: %w", s.step.Name, err)
				cancel() // 中断其余分支
			})
			return
		}
		mu.Lock()
		vars[s.step.Name] = out
		var next []int
		for _, j := range s.dependents {
			if indeg[j]--; indeg[j] == 0 {
				next = append(next, j)
			}
		}
		mu.Unlock()
		for _, j := range next {
			wg.Add(1)
			go exec(j)
		}
	}
	// 先收集入度为 0 的起点,再统一起 goroutine:避免这里读 indeg 与
	// 已启动分支在锁内递减 indeg(见 exec)并发,构成数据竞争。
	var roots []int
	for i, d := range indeg {
		if d == 0 {
			roots = append(roots, i)
		}
	}
	for _, i := range roots {
		wg.Add(1)
		go exec(i)
	}
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}

	mu.Lock()
	defer mu.Unlock()
	return vars[p.steps[p.output].step.Name], nil
}

// runStep 执行单个步骤,带声明的超时与重试。步骤超时视为失败
// (强保证流程的确定性中断),与主循环工具面的"超时回传消息"不同。
func runStep(ctx context.Context, s *compiledStep, args string) (string, error) {
	attempts := s.step.Retry + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return "", lastErr
			case <-t.C:
			}
		}
		out, err := invokeStep(ctx, s, args)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

func invokeStep(ctx context.Context, s *compiledStep, args string) (string, error) {
	if s.step.Context == "fork" {
		ctx = runctx.WithForkContext(ctx) // 目标能力以调用方对话快照起步
	}
	d := s.step.Timeout.Std()
	if d <= 0 {
		return capability.Invoke(ctx, s.cap, args)
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := capability.Invoke(tctx, s.cap, args)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-tctx.Done():
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("timed out after %s", d)
	}
}

// renderVars 单次扫描渲染模板:每个 {ident} 查一次表,命中即替换,
// 未命中原样保留。不做多轮替换,步骤输出里出现的占位字面量不会被
// 二次展开(确定性优先)。
//
// JSON 安全:替换时跟踪模板自身的字符串上下文——占位符落在 JSON
// 字符串内(如 '{"q":"{plan}"}')时,值自动做 JSON 转义,上游输出含
// 引号/换行不会破坏下游的参数解析;落在字符串外(纯文本提示词、
// 数字/对象位置)则原样注入。
func renderVars(tpl string, vars map[string]string) string {
	if tpl == "" {
		return ""
	}
	locs := tplRef.FindAllStringSubmatchIndex(tpl, -1)
	if locs == nil {
		return tpl
	}
	var sb strings.Builder
	inString := false // 扫描模板字面量维护的 JSON 字符串状态
	prev := 0
	for _, loc := range locs {
		lit := tpl[prev:loc[0]]
		sb.WriteString(lit)
		inString = advanceStringState(inString, lit)
		key := tpl[loc[2]:loc[3]]
		if v, ok := vars[key]; ok {
			if inString {
				sb.WriteString(jsonEscape(v))
			} else {
				sb.WriteString(v)
			}
		} else {
			sb.WriteString(tpl[loc[0]:loc[1]]) // 未知占位原样保留
		}
		prev = loc[1]
	}
	sb.WriteString(tpl[prev:])
	return sb.String()
}

// advanceStringState 沿模板字面量推进 JSON 字符串开合状态
// (跳过转义的 \" )。
func advanceStringState(in bool, lit string) bool {
	for i := 0; i < len(lit); i++ {
		switch lit[i] {
		case '\\':
			if in {
				i++ // 字符串内的转义序列,跳过下一字符
			}
		case '"':
			in = !in
		}
	}
	return in
}

// jsonEscape 返回 s 作为 JSON 字符串内容的转义形式(不含首尾引号)。
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b[1 : len(b)-1])
}
