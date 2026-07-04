// Package workflow 是强保证场景的逃生舱:审批流、合规流程、高频路径
// 不指望大脑自觉——步骤用 AsLambda 编进 compose.Graph 钉死,没有大脑
// 做选择,orchestrator 就是这张图,模型只在图里的固定节点上出现。
//
// 产物同样是 capability.Capability(cap://skill/...):既可以
// 独立服务,也可以作为一个工具挂回某个大脑的工具面——静态编排与
// 动态编排通过双形态抽象互相嵌套。
package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/source"
)

// Step 声明图上的一个固定节点。
type Step struct {
	Name string `yaml:"name"`
	// Use 是能力引用(cap://...,精确)或保留字 "model"(调用工作流模型)。
	Use string `yaml:"use"`
	// Args 是入参模板:能力步骤渲染为参数 JSON,model 步骤渲染为提示词。
	// 可引用 {input}(工作流输入)与 {<步骤名>}(之前步骤的输出)。
	Args string `yaml:"args"`
}

// Config 声明一个工作流。
type Config struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Steps       []Step `yaml:"steps"`
}

type wfState struct {
	vars map[string]string
}

// Build 把声明编译为 compose.Graph 并包装成能力。
// 所有能力引用在编译时解析并锁定,运行期不查目录。
func Build(ctx context.Context, cfg Config, catalog *source.Catalog, m model.ToolCallingChatModel) (capability.Capability, error) {
	if cfg.Name == "" || len(cfg.Steps) == 0 {
		return nil, fmt.Errorf("workflow: name and steps are required")
	}

	g := compose.NewGraph[string, string](compose.WithGenLocalState(func(ctx context.Context) *wfState {
		return &wfState{vars: map[string]string{}}
	}))

	// 入口节点:把工作流输入存入状态。
	const inputNode = "__input"
	err := g.AddLambdaNode(inputNode,
		compose.InvokableLambda(func(ctx context.Context, in string) (string, error) { return in, nil }),
		compose.WithStatePostHandler(func(ctx context.Context, out string, s *wfState) (string, error) {
			s.vars["input"] = out
			return out, nil
		}))
	if err != nil {
		return nil, err
	}

	prev := inputNode
	for _, step := range cfg.Steps {
		if step.Name == "" || step.Use == "" {
			return nil, fmt.Errorf("workflow %s: every step needs name and use", cfg.Name)
		}
		lambda, err := stepLambda(ctx, step, catalog, m)
		if err != nil {
			return nil, fmt.Errorf("workflow %s step %s: %w", cfg.Name, step.Name, err)
		}
		name, args := step.Name, step.Args
		err = g.AddLambdaNode(name, lambda,
			// 前置:用状态渲染入参模板(忽略上一节点的直连输出,数据流经状态)。
			compose.WithStatePreHandler(func(ctx context.Context, _ string, s *wfState) (string, error) {
				return render(args, s.vars), nil
			}),
			// 后置:存储本步输出,后续步骤以 {步骤名} 引用。
			compose.WithStatePostHandler(func(ctx context.Context, out string, s *wfState) (string, error) {
				s.vars[name] = out
				return out, nil
			}))
		if err != nil {
			return nil, err
		}
		if err := g.AddEdge(prev, name); err != nil {
			return nil, err
		}
		prev = name
	}
	if err := g.AddEdge(compose.START, inputNode); err != nil {
		return nil, err
	}
	if err := g.AddEdge(prev, compose.END); err != nil {
		return nil, err
	}

	runnable, err := g.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: compile: %w", cfg.Name, err)
	}

	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "skill", Domain: "workflows", Name: cfg.Name},
		Description: cfg.Description,
		Params:      capability.SingleParam("input", "工作流输入"),
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		input := capability.ParseSingle(argsJSON, "input")
		return runnable.Invoke(ctx, input)
	}), nil
}

func stepLambda(ctx context.Context, step Step, catalog *source.Catalog, m model.ToolCallingChatModel) (*compose.Lambda, error) {
	if step.Use == "model" {
		if m == nil {
			return nil, fmt.Errorf("step uses model but workflow has no model")
		}
		return compose.InvokableLambda(func(ctx context.Context, prompt string) (string, error) {
			out, err := m.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
			if err != nil {
				return "", err
			}
			return out.Content, nil
		}), nil
	}
	c, err := catalog.Get(step.Use)
	if err != nil {
		return nil, err
	}
	return c.AsLambda(ctx)
}

func render(tpl string, vars map[string]string) string {
	if tpl == "" {
		return vars["input"]
	}
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
