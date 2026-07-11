package capability

import (
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// ParamDecl 描述一个可调用单元(skill / component / 编排步骤)的入参。
// 它是「可调用单元的参数接口」,循环族与编排族共用,故归基座 capability。
type ParamDecl struct {
	Type     string `yaml:"type" json:"type"`
	Desc     string `yaml:"desc" json:"desc"`
	Required bool   `yaml:"required" json:"required"`
}

// ParamsSchema 把 ParamDecl 映射为工具入参 schema;无参数时退化为单个
// string 入参 {"input": ...}(与 capability.New 的默认一致)。
// type 词汇装配期锁死:int/str/bool 等拼法此前静默转 string,模型按错
// 类型传参、后端拒收,错误面在运行期且难归因。
func ParamsSchema(params map[string]ParamDecl) (*schema.ParamsOneOf, error) {
	if len(params) == 0 {
		return SingleParam("input", "输入内容"), nil
	}
	out := make(map[string]*schema.ParameterInfo, len(params))
	for name, p := range params {
		typ := schema.String
		switch p.Type {
		case "", "string":
		case "number":
			typ = schema.Number
		case "integer":
			typ = schema.Integer
		case "boolean":
			typ = schema.Boolean
		case "array", "object":
			return nil, fmt.Errorf("param %s: type %q is not supported by the flat param declaration (declare the field as string and document the JSON shape in desc)", name, p.Type)
		default:
			return nil, fmt.Errorf("param %s: unknown type %q (want string|number|integer|boolean)", name, p.Type)
		}
		out[name] = &schema.ParameterInfo{Type: typ, Desc: p.Desc, Required: p.Required}
	}
	return schema.NewParamsOneOfByParams(out), nil
}
