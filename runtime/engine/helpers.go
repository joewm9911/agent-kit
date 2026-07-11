package engine

import (
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// unmarshalLoose 容忍模型在 JSON 外包裹推理块/说明文字/代码栏后解析。
// 用按值解码而非整段 Unmarshal:只消费第一个完整 JSON 值,尾部冗余
// (真机实测有模型会多打一个 })不致解析失败。
func unmarshalLoose(s string, target any) error {
	return json.NewDecoder(strings.NewReader(ExtractJSON(s))).Decode(target)
}

// singleAsStream 把单条结果转为流返回,供中间过程不适合流式的引擎
// 实现 Stream(过程可见性走 observe 轨迹)。
func singleAsStream(out *schema.Message, err error) (*schema.StreamReader[*schema.Message], error) {
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

// stepRoundsDefault 是执行器子循环轮数的缺省:优先取执行画像的
// loop.max_rounds(asm.MaxSteps)——组件上配了它却只对 react 生效、
// 对 plan-execute/reflection 静默无效,是审计 P2-2 抓的一词两义坑;
// engine_config.step_max_rounds 显式声明时仍最优先。
func stepRoundsDefault(asm *Assembly) int {
	if asm.MaxSteps > 0 {
		return asm.MaxSteps
	}
	return 10
}

// hasTag 报告 tags 是否含 want。
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
