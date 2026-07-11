// Package capability 定义框架的最小统一抽象:一切节点皆能力。
//
// 工具、ChatModel、Memory、RAG、Skill、完整的 Agent,都实现同一个
// Capability 接口。每个能力有两种挂载形态:
//
//   - AsTool:挂到模型的工具面,由"大脑"(LLM)决定何时调用 —— 动态编排;
//   - AsLambda:挂到 eino Graph 的固定节点,由流程决定何时执行 —— 静态编排。
//
// 能力以 Ref(cap://kind/ns/name@version)标识,携带 Risk
// 分级供运行时做审批拦截与准入控制。
package capability

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// Risk 是能力的风险分级,是审批拦截与目录准入的依据。
//
// 零值是 RiskUnspecified 而非 readonly:忘写 Risk 的 mutating 能力若默认
// 最宽松,会静默绕过审批闸门。门闸与准入对 Unspecified 按 mutating 保守
// 对待(经 Effective);内置只读工具全部显式标注 RiskReadonly。
type Risk int

const (
	RiskUnspecified Risk = iota // 未声明:门闸按 mutating 保守对待
	RiskReadonly                // 只读,无副作用
	RiskMutating                // 有改动性副作用(写文件、发消息、下单)
	RiskDangerous               // 危险(删除、资金、不可逆),默认不入目录
)

// Effective 返回门闸/准入视角的等效级别:未声明按 mutating 保守处理。
func (r Risk) Effective() Risk {
	if r == RiskUnspecified {
		return RiskMutating
	}
	return r
}

func (r Risk) String() string {
	switch r {
	case RiskReadonly:
		return "readonly"
	case RiskMutating:
		return "mutating"
	case RiskDangerous:
		return "dangerous"
	default:
		return "unspecified"
	}
}

// ParseRisk 解析风险级别字符串,空串按 readonly 处理。
func ParseRisk(s string) (Risk, error) {
	switch s {
	case "", "readonly":
		return RiskReadonly, nil
	case "mutating":
		return RiskMutating, nil
	case "dangerous":
		return RiskDangerous, nil
	default:
		return 0, fmt.Errorf("unknown risk level %q", s)
	}
}

// TagInteractive 标记"阻塞等待用户输入"的交互类能力(ask_user 等):
// 工具超时闸门对其豁免——等人回复的时间不是执行时间。
const TagInteractive = "interactive"

// TagRawResult 标记能力的结果不参与消化(结果本身就是给模型的原文,
// 如 read_result 的分页输出)。声明于 core:消化闸门(loop)与引擎的
// 计划面筛选(engine)都要认它,而 engine 不得依赖 loop。
const TagRawResult = "result:raw"

// Meta 是能力的自描述清单。Description 会作为工具描述暴露给模型,
// 是大脑调用决策的直接依据,写得越清楚决策越准。
type Meta struct {
	Ref         Ref
	Description string
	// Params 是工具形态的入参 schema;为 nil 时默认单个 string 入参 {"input": ...}。
	Params *schema.ParamsOneOf
	Risk   Risk
	Tags   []string
}

// Capability 是所有节点的统一抽象。
type Capability interface {
	Meta() Meta
	// AsTool 返回能力的工具形态,供模型自主调用。
	AsTool(ctx context.Context) (tool.BaseTool, error)
	// AsLambda 返回能力的图节点形态(string 入参 JSON -> string 出参),
	// 供 compose.Graph 静态编排。
	AsLambda(ctx context.Context) (*compose.Lambda, error)
}

// InvokeFunc 是能力的统一执行契约:入参为 JSON 字符串,出参为字符串。
type InvokeFunc func(ctx context.Context, argsJSON string) (string, error)

// NoParams 显式声明"该工具确实无入参":工具形态的 ToolInfo.ParamsOneOf
// 置为 nil——eino FAQ 明确无参工具必须传 nil,空 schema 会被部分厂商
// 拒绝(400)。仅作身份标记,不要调用其方法。
var NoParams = &schema.ParamsOneOf{}

// New 从一个执行函数构造能力,是自定义能力最直接的入口。
func New(meta Meta, fn InvokeFunc) Capability {
	if meta.Params == nil {
		meta.Params = schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"input": {Type: schema.String, Desc: "输入内容", Required: true},
		})
	}
	return &funcCapability{meta: meta, fn: fn}
}

type funcCapability struct {
	meta Meta
	fn   InvokeFunc
}

func (c *funcCapability) Meta() Meta { return c.meta }

func (c *funcCapability) AsTool(ctx context.Context) (tool.BaseTool, error) {
	return &funcTool{meta: c.meta, fn: c.fn, name: c.meta.Ref.Name}, nil
}

func (c *funcCapability) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return compose.InvokableLambda(func(ctx context.Context, argsJSON string) (string, error) {
		return c.fn(ctx, argsJSON)
	}), nil
}

// funcTool 是能力工具形态的通用实现,name 是模型可见的短名
// (可能因撞名被目录升级,与 Ref.Name 不同)。
type funcTool struct {
	meta Meta
	fn   InvokeFunc
	name string
}

func (t *funcTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	params := t.meta.Params
	if params == NoParams {
		params = nil // 无参工具:ParamsOneOf 必须为 nil(见 NoParams)
	}
	return &schema.ToolInfo{
		Name:        t.name,
		Desc:        t.meta.Description,
		ParamsOneOf: params,
	}, nil
}

func (t *funcTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	return t.fn(ctx, argsJSON)
}

// FromTool 把一个已有的 eino 工具(如 MCP 拉取的工具)包装成能力。
// ref 的 Name 为空时取工具自报的名字。构造时调用一次 Info 获取元信息。
func FromTool(ctx context.Context, t tool.BaseTool, ref Ref, risk Risk) (Capability, error) {
	info, err := t.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tool info: %w", err)
	}
	inv, ok := t.(tool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q is not invokable", info.Name)
	}
	if ref.Name == "" {
		ref.Name = info.Name
	}
	if ref.Kind == "" {
		ref.Kind = "tool"
	}
	params := info.ParamsOneOf
	if params == nil {
		// 外部工具(MCP 等)合法无参:nil 若透传,New 会注入默认的
		// {input: required} 假 schema——模型对无参工具硬造参数、被拒。
		// 映射为 NoParams 哨兵,工具形态按厂商契约传 nil schema。
		params = NoParams
	}
	meta := Meta{Ref: ref, Description: info.Desc, Params: params, Risk: risk}
	return New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		return inv.InvokableRun(ctx, argsJSON)
	}), nil
}

// Rename 返回一个模型可见短名不同的能力副本,目录在撞名升级时使用。
// Ref 与其余元信息不变。
func Rename(c Capability, toolName string) Capability {
	return &renamed{inner: c, toolName: toolName}
}

type renamed struct {
	inner    Capability
	toolName string
}

func (r *renamed) Meta() Meta { return r.inner.Meta() }

func (r *renamed) AsTool(ctx context.Context) (tool.BaseTool, error) {
	meta := r.inner.Meta()
	return &funcTool{meta: meta, name: r.toolName, fn: func(ctx context.Context, argsJSON string) (string, error) {
		return Invoke(ctx, r.inner, argsJSON)
	}}, nil
}

func (r *renamed) AsLambda(ctx context.Context) (*compose.Lambda, error) {
	return r.inner.AsLambda(ctx)
}

// Invoke 以统一契约直接调用一个能力(工具形态的执行路径)。
func Invoke(ctx context.Context, c Capability, argsJSON string) (string, error) {
	t, err := c.AsTool(ctx)
	if err != nil {
		return "", err
	}
	inv, ok := t.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("capability %s is not invokable", c.Meta().Ref)
	}
	return inv.InvokableRun(ctx, argsJSON)
}

// AsTools 批量转换能力为 eino 工具,供 ToolsNode / ReAct 使用。
func AsTools(ctx context.Context, caps []Capability) ([]tool.BaseTool, error) {
	tools := make([]tool.BaseTool, 0, len(caps))
	for _, c := range caps {
		t, err := c.AsTool(ctx)
		if err != nil {
			return nil, fmt.Errorf("capability %s as tool: %w", c.Meta().Ref, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// SingleParam 生成单参数的入参 schema,减少样板代码。
func SingleParam(name, desc string) *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		name: {Type: schema.String, Desc: desc, Required: true},
	})
}

// ParseSingle 从参数 JSON 里取出单个字符串字段;解析失败时把整个
// 入参当作该字段的值(容忍图节点直接传裸字符串)。
func ParseSingle(argsJSON, field string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err == nil {
		if v, ok := m[field]; ok {
			return fmt.Sprint(v)
		}
	}
	return argsJSON
}
