// Package skill 实现"结构进能力"的正式载体:skill 是
// 「任务书模板 + 参数 schema + 执行引擎 + 工具子集 + 可选专属模型」
// 的打包单元。打包产物实现 capability.Capability,上级大脑只看到
// 一个描述清晰的工具;内部是单次调用、小 ReAct 循环还是 plan-execute,
// 取决于声明的重量,机制完全相同。
//
// 三条在装配时固定的边界:
//   - 接口边界:大脑只看到 description + params,引擎的存在被隐藏;
//   - 上下文边界:每次调用从零起一轮,内部消息不回流宿主上下文;
//   - 权限边界:工具面被锁定为声明的子集,不继承宿主能力。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/prompt"
	"github.com/joewm9911/agent-kit/registry"
	"github.com/joewm9911/agent-kit/source"
	"github.com/joewm9911/agent-kit/suspend"
)

// ParamDecl 描述一个 skill 参数。
type ParamDecl struct {
	Type     string `yaml:"type" json:"type"`
	Desc     string `yaml:"desc" json:"desc"`
	Required bool   `yaml:"required" json:"required"`
}

// ModelDecl 是 skill 专属模型声明,nil 则跟随宿主。
type ModelDecl struct {
	Provider string         `yaml:"provider" json:"provider"`
	Config   map[string]any `yaml:"config" json:"config"`
}

// Declaration 是一个 skill 的完整声明(可来自 YAML 或代码)。
type Declaration struct {
	// Name 形如 "research/competitor_report",namespace/name。
	Name        string               `yaml:"name"`
	Version     string               `yaml:"version"`
	Description string               `yaml:"description"`
	Params      map[string]ParamDecl `yaml:"params"`
	// Prompt 是任务书模板(业务知识所在),params 以 {name} 占位渲染。
	Prompt prompt.Value `yaml:"prompt"`
	// Engine 是执行引擎:react(默认)| plan-execute | 已注册的自定义模板。
	Engine string `yaml:"engine"`
	// EngineConfig 透传引擎专属配置;*_prompt 键支持字面量或 {ref: ...}。
	EngineConfig map[string]any `yaml:"engine_config"`
	// Capabilities 是内部工具面(最小权限子集),CapRef 模式。
	Capabilities struct {
		Include []string `yaml:"include"`
		Exclude []string `yaml:"exclude"`
	} `yaml:"capabilities"`
	Model    *ModelDecl `yaml:"model"`
	MaxSteps int        `yaml:"max_steps"`
	// Compaction 启用内部循环的上下文压缩(长任务 skill 建议开启)。
	Compaction loop.CompactionConfig `yaml:"compaction"`
}

// Deps 是装配 skill 所需的环境。
type Deps struct {
	Catalog      *source.Catalog
	Prompts      *prompt.Resolver
	DefaultModel model.ToolCallingChatModel
	// LoopPrompt 是 L1 框架规约,skill 内部小循环复用,保持运行纪律一致。
	LoopPrompt string
	// Capabilities 是预解析的工具面。非空时跳过 Catalog 选品,由调用方
	// (命名空间装配层)负责引用解析与边界校验。
	Capabilities []capability.Capability
	// ToolTimeout 是内部工具面的单次调用超时(0 默认,<0 关闭)。
	ToolTimeout time.Duration
	// Retry 是 skill 专属模型的瞬时错误重试策略(DefaultModel 由上层包装)。
	Retry loop.RetryConfig
}

// Build 把声明装配为能力:解析全部引用并锁版本 → 检查依赖 →
// 构建引擎 Runner → 套上 manifest。
func Build(ctx context.Context, decl *Declaration, deps Deps) (capability.Capability, error) {
	ns, name, err := splitName(decl.Name)
	if err != nil {
		return nil, err
	}
	engineName := decl.Engine
	if engineName == "" {
		engineName = "react"
	}

	// 模型:专属或跟随宿主。专属模型在此套 Ring 0 中间件
	// (重试 + 预算,预算门闸经 ctx 生效);DefaultModel 由上层包装。
	m := deps.DefaultModel
	if decl.Model != nil {
		if m, err = registry.BuildModel(ctx, decl.Model.Provider, decl.Model.Config); err != nil {
			return nil, fmt.Errorf("skill %s: build model: %w", decl.Name, err)
		}
		m = loop.BudgetModel(loop.RetryModel(m, deps.Retry))
	}
	if m == nil {
		return nil, fmt.Errorf("skill %s: no model (declare model or provide default)", decl.Name)
	}

	// 工具子集:预解析优先(命名空间装配);否则按 CapRef 从目录选品,
	// 依赖解析失败即拒绝装配(fail fast,不等大脑调用时才炸)。
	caps := deps.Capabilities
	if caps == nil && len(decl.Capabilities.Include) > 0 {
		caps, err = deps.Catalog.Select(decl.Capabilities.Include, decl.Capabilities.Exclude)
		if err != nil {
			return nil, fmt.Errorf("skill %s: select capabilities: %w", decl.Name, err)
		}
		if err := checkExactRefs(decl.Capabilities.Include, caps); err != nil {
			return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
		}
	}

	// 风险传播:skill 的有效风险 = 绑定能力风险的最大值
	risk := capability.RiskReadonly
	for _, c := range caps {
		if r := c.Meta().Risk; r > risk {
			risk = r
		}
	}

	// 任务书模板与引擎提示词:此刻解析、锁版本
	brief, err := decl.Prompt.Resolve(ctx, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("skill %s: resolve prompt: %w", decl.Name, err)
	}
	prompts, engineConf, err := resolveEnginePrompts(ctx, decl.EngineConfig, deps.Prompts)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", decl.Name, err)
	}

	// 内部工具面下沉全部 Ring 0 闸门(治理不再止步于 agent 主循环):
	// 超时(最内,只计执行时间)→ 截断 → 效果日志 → 审批(最外,批准
	// 等待不占超时)。审批模式、预算门闸与挂起日志经调用方 ctx 生效,
	// 同一 skill 被不同策略的 agent 复用时各自独立。
	caps = loop.TimeoutTools(caps, deps.ToolTimeout)
	caps = loop.TruncateResults(caps, 0)
	caps = suspend.DurableEffects(caps)
	caps = loop.GateApprovalCtx(caps)

	runner, err := engine.Build(ctx, engineName, &engine.Assembly{
		Model:        m,
		Capabilities: caps,
		MaxSteps:     decl.MaxSteps,
		Modifier:     loop.PromptLayers{Loop: deps.LoopPrompt}.Modifier(),
		Rewriter:     loop.Compactor(m, decl.Compaction), // 内部长循环可压缩
		Prompts:      prompts,
		Config:       engineConf,
	})
	if err != nil {
		return nil, fmt.Errorf("skill %s: build engine %s: %w", decl.Name, engineName, err)
	}

	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "skill", Provider: engineName, Namespace: ns, Name: name, Version: decl.Version},
		Description: decl.Description,
		Params:      paramsSchema(decl.Params),
		Risk:        risk,
		Tags:        []string{"prompt:" + brief.Version},
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		vars := map[string]string{}
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
			for k, v := range args {
				vars[k] = fmt.Sprint(v)
			}
		} else {
			vars["input"] = argsJSON
		}
		task := brief.Render(vars)
		// 上下文边界:独立会话,内部过程不回流宿主,只返回最终结果。
		out, err := runner.Generate(ctx, []*schema.Message{schema.UserMessage(task)})
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}), nil
}

func splitName(full string) (ns, name string, err error) {
	i := strings.LastIndex(full, "/")
	if i < 0 {
		return "skills", full, nil
	}
	ns, name = full[:i], full[i+1:]
	if ns == "" || name == "" {
		return "", "", fmt.Errorf("skill: bad name %q, want namespace/name", full)
	}
	return ns, name, nil
}

// checkExactRefs 确保不含通配符的精确引用都被解析到,漏一个即失败。
func checkExactRefs(include []string, selected []capability.Capability) error {
	for _, pat := range include {
		if strings.Contains(pat, "*") {
			continue
		}
		ref, err := capability.ParseRef(pat)
		if err != nil {
			return err
		}
		found := false
		for _, c := range selected {
			if c.Meta().Ref.Match(ref) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("dependency %s not found in catalog", pat)
		}
	}
	return nil
}

// resolveEnginePrompts 把 engine_config 中 *_prompt 键解析为文本
// (支持字面量与 {ref: cap://prompt...}),其余键原样透传给引擎。
func resolveEnginePrompts(ctx context.Context, conf map[string]any, r *prompt.Resolver) (map[string]string, map[string]any, error) {
	prompts := map[string]string{}
	rest := map[string]any{}
	for k, v := range conf {
		if !strings.HasSuffix(k, "_prompt") {
			rest[k] = v
			continue
		}
		key := strings.TrimSuffix(k, "_prompt")
		switch val := v.(type) {
		case string:
			prompts[key] = val
		case map[string]any:
			refStr, _ := val["ref"].(string)
			if refStr == "" {
				return nil, nil, fmt.Errorf("engine_config.%s: expect string or {ref: ...}", k)
			}
			if r == nil {
				return nil, nil, fmt.Errorf("engine_config.%s: prompt ref used but no prompt sources configured", k)
			}
			tpl, err := r.Resolve(ctx, refStr)
			if err != nil {
				return nil, nil, err
			}
			prompts[key] = tpl.Text
		default:
			return nil, nil, fmt.Errorf("engine_config.%s: unsupported value type", k)
		}
	}
	return prompts, rest, nil
}

func paramsSchema(params map[string]ParamDecl) *schema.ParamsOneOf {
	if len(params) == 0 {
		return capability.SingleParam("input", "输入内容")
	}
	out := make(map[string]*schema.ParameterInfo, len(params))
	for name, p := range params {
		typ := schema.String
		switch p.Type {
		case "number":
			typ = schema.Number
		case "integer":
			typ = schema.Integer
		case "boolean":
			typ = schema.Boolean
		}
		out[name] = &schema.ParameterInfo{Type: typ, Desc: p.Desc, Required: p.Required}
	}
	return schema.NewParamsOneOfByParams(out)
}
