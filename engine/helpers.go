package engine

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

// unmarshalLoose 容忍模型在 JSON 外包裹说明文字/代码块标记后解析。
func unmarshalLoose(s string, target any) error {
	return json.Unmarshal([]byte(ExtractJSON(s)), target)
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
