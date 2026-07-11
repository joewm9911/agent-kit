// Package engine 提供执行引擎模板(Ring 1)。引擎是"循环结构"的代码
// 实现:react 是唯一的主循环形态,plan-execute 等其他模板不直接面向
// 用户,而是被 skill 声明引用、打包成能力挂到工具面上。
// 新循环形态(reflection、debate)在此注册后即可被 skill 配置引用。
package engine

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
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
	var names []string
	if !ok { // 名字列表在锁内抄出:错误路径在锁外遍历注册表是数据竞争
		for k := range builders {
			names = append(names, k)
		}
	}
	mu.RUnlock()
	if !ok {
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

var (
	// thinkBlockRe 匹配推理模型内联在 content 里的思考块(MiniMax-M2/M1、
	// DeepSeek-R1 等经 OpenAI 兼容接口时的形态)。思考文本里常出现花括号
	// ({eN} 引用示例、示范 JSON),必须先整块剥除,否则按括号定位会取偏。
	thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)
	// fencedRe 匹配第一个 ``` 代码栏(可带 json 语言标)。
	fencedRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")
)

// ExtractJSON 从模型输出中提取 JSON 文本,按包裹形态逐层剥离:
// 先剥 <think> 推理块,再优先取 ``` 代码栏内容,最后截取第一个 { 到
// 最后一个 } 之间。注意产物可能带尾部冗余(模型多打括号),解析方应
// 按值解码(见 unmarshalLoose),不要用严格的整段 Unmarshal。
func ExtractJSON(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	if m := fencedRe.FindStringSubmatch(s); m != nil && strings.Contains(m[1], "{") {
		s = m[1]
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
