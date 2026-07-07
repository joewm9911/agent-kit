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
