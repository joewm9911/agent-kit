package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
)

func init() {
	Register("router", BuildRouter)
}

const defaultRoutePrompt = `你是路由器。根据输入从下列目标中选择唯一最合适的一个,并为它组织调用参数。
只输出 JSON:{"target": "<目标名>", "args": {<按目标的参数说明组织>}}
不要回答问题本身,不要选多个目标。`

// BuildRouter 构建分诊引擎:一次轻量模型调用把输入路由到工具面上的
// 某个能力(能力的 description 即路由说明),调用后直接返回其结果——
// 共一次模型调用 + 一次能力调用,没有循环。
//
// 用它替代"主脑 react 隐式路由":成本可控、路由行为可审计(选了谁、
// 为什么在轨迹里一目了然)。路由表就是 component 声明的 tools 列表,
// 不需要额外配置。
//
// engine_config:
//
//	fallback       模型选择无法解析/目标不存在时的兜底能力名(可选;
//	               未配置时报错)
//	route_prompt   覆盖内置路由提示词
func BuildRouter(ctx context.Context, asm *Assembly) (Runner, error) {
	if len(asm.Capabilities) == 0 {
		return nil, fmt.Errorf("router: no capabilities to route to")
	}
	targets := make(map[string]capability.Capability, len(asm.Capabilities))
	var sb strings.Builder
	for _, c := range asm.Capabilities {
		meta := c.Meta()
		targets[meta.Ref.Name] = c
		fmt.Fprintf(&sb, "- %s: %s\n", meta.Ref.Name, meta.Description)
	}
	fallback := asm.ConfString("fallback", "")
	if fallback != "" {
		if _, ok := targets[fallback]; !ok {
			return nil, fmt.Errorf("router: fallback %q is not on the tool face", fallback)
		}
	}
	return &routerRunner{
		asm:      asm,
		targets:  targets,
		listing:  sb.String(),
		prompt:   promptOr(asm, "route", defaultRoutePrompt),
		fallback: fallback,
	}, nil
}

type routerRunner struct {
	asm      *Assembly
	targets  map[string]capability.Capability
	listing  string
	prompt   string
	fallback string
}

type routeDecision struct {
	Target string         `json:"target"`
	Args   map[string]any `json:"args"`
}

func (r *routerRunner) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	task := renderConversation(msgs)

	out, err := r.asm.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(r.prompt + "\n\n可选目标:\n" + r.listing),
		schema.UserMessage(task),
	})
	if err != nil {
		return nil, fmt.Errorf("router decide: %w", err)
	}

	var d routeDecision
	target := capability.Capability(nil)
	args := ""
	if err := unmarshalLoose(out.Content, &d); err == nil {
		if c, ok := r.targets[d.Target]; ok {
			target = c
			if b, err := json.Marshal(d.Args); err == nil {
				args = string(b)
			}
		}
	}
	if target == nil { // 解析失败或目标不存在:兜底或报错
		if r.fallback == "" {
			return nil, fmt.Errorf("router: cannot resolve route from %q (declare fallback to absorb this)", out.Content)
		}
		target = r.targets[r.fallback]
		b, _ := json.Marshal(map[string]string{"input": task})
		args = string(b)
	}

	result, err := capability.Invoke(ctx, target, args)
	if err != nil {
		return nil, fmt.Errorf("router: invoke %s: %w", target.Meta().Ref.Name, err)
	}
	return schema.AssistantMessage(result, nil), nil
}

func (r *routerRunner) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return singleAsStream(r.Generate(ctx, msgs))
}
