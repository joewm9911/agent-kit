// Package engine 提供执行引擎模板(Ring 1)。引擎是"循环结构"的代码
// 实现:react 是唯一的主循环形态,plan-execute 等其他模板不直接面向
// 用户,而是被 skill 声明引用、打包成能力挂到工具面上。
// 新循环形态(reflection、debate)在此注册后即可被 skill 配置引用。
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
)

// MessageModifier 与 eino react 的同名概念对齐:模型调用前修改消息。
type MessageModifier func(ctx context.Context, msgs []*schema.Message) []*schema.Message

// Assembly 是构建一个引擎实例所需的全部原料,由 agent/skill 构建层装配。
type Assembly struct {
	Model        model.ToolCallingChatModel
	Capabilities []capability.Capability
	MaxSteps     int
	// Modifier 在每次模型调用前注入 system prompt(L1-L4 拼装的产物)。
	Modifier MessageModifier
	// Rewriter 在每次模型调用前重写累积历史(上下文压缩)。
	Rewriter MessageModifier
	// Prompts 是引擎专属提示词(如 planner/replanner),已渲染为文本。
	Prompts map[string]string
	// Config 是引擎专属配置(如 max_rounds)。
	Config map[string]any
}

// Runner 是引擎构建产物的统一执行面。
type Runner interface {
	Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error)
	Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error)
}

// Builder 按装配单构建 Runner。
type Builder func(ctx context.Context, asm *Assembly) (Runner, error)

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

// Register 注册一个引擎模板。
func Register(name string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := builders[name]; ok {
		panic(fmt.Sprintf("engine: %q already registered", name))
	}
	builders[name] = b
}

// Build 按名称构建 Runner,name 为空默认 react。
func Build(ctx context.Context, name string, asm *Assembly) (Runner, error) {
	if name == "" {
		name = "react"
	}
	mu.RLock()
	b, ok := builders[name]
	mu.RUnlock()
	if !ok {
		names := make([]string, 0, len(builders))
		for n := range builders {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("engine: unknown engine %q, registered: %v", name, names)
	}
	return b(ctx, asm)
}

// ConfInt 从引擎配置取整数,缺省返回 def。
func (a *Assembly) ConfInt(key string, def int) int {
	if v, ok := a.Config[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return def
}

// ConfString 从引擎配置取字符串,缺省返回 def。
func (a *Assembly) ConfString(key, def string) string {
	if v, ok := a.Config[key].(string); ok && v != "" {
		return v
	}
	return def
}

// ExtractJSON 截取输出中第一个 { 到最后一个 } 之间的内容,
// 容忍模型在 JSON 外包裹说明文字或代码块标记。
func ExtractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
