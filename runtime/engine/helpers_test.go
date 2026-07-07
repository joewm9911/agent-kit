package engine

import "testing"

// TestUnmarshalLooseReasoningModel:推理模型(MiniMax-M2 真机)输出形态的
// 解析回归——三个夹具都是 TestLiveEnginePrompts 抓到的实际失败输出的
// 缩影:<think> 块内含花括号/示例 JSON、真实 JSON 在 ``` 代码栏、
// 尾部多打一个 }。
func TestUnmarshalLooseReasoningModel(t *testing.T) {
	// 夹具1(rewoo planner):think 内含 {e1} 引用示意,首个 { 在思考块里。
	rewooOut := "<think>\n步骤:\n1. e1: 调用 get_top_seller\n2. e2: 使用 {e1} 返回的商品ID查询库存\n</think>\n\n```json\n{\n  \"steps\": [\n    {\"id\": \"e1\", \"tool\": \"get_top_seller\", \"args\": {}},\n    {\"id\": \"e2\", \"tool\": \"get_inventory\", \"args\": {\"product_id\": \"{e1}\"}}\n  ]\n}\n```"
	var plan struct {
		Steps []struct {
			ID   string         `json:"id"`
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		} `json:"steps"`
	}
	if err := unmarshalLoose(rewooOut, &plan); err != nil {
		t.Fatalf("rewoo fixture: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[1].Args["product_id"] != "{e1}" {
		t.Fatalf("rewoo fixture parsed wrong: %+v", plan)
	}

	// 夹具2(plan-execute replanner):think 内含完整示例 JSON,真实 JSON
	// 在代码栏且尾部多一个 }(模型笔误,真机原样)。
	replanOut := "<think>\n目标已达成,我应该输出:\n{\"action\": \"finish\", \"response\": \"给用户的最终回答\"}\n</think>\n\n```json\n{\"action\": \"finish\", \"response\": \"今天北京天气晴,28°C。\"}}\n```"
	var v struct {
		Action   string `json:"action"`
		Response string `json:"response"`
	}
	if err := unmarshalLoose(replanOut, &v); err != nil {
		t.Fatalf("replan fixture: %v", err)
	}
	if v.Action != "finish" || v.Response == "" {
		t.Fatalf("replan fixture parsed wrong: %+v", v)
	}

	// 夹具3(router):think 内含示例 JSON,真实 JSON 在代码栏。
	routeOut := "<think>\n最终输出应该是:\n{\"target\": \"query_refunds\", \"args\": {}}\n</think>\n\n```json\n{\"target\": \"query_refunds\", \"args\": {}}\n```"
	var d struct {
		Target string         `json:"target"`
		Args   map[string]any `json:"args"`
	}
	if err := unmarshalLoose(routeOut, &d); err != nil {
		t.Fatalf("route fixture: %v", err)
	}
	if d.Target != "query_refunds" {
		t.Fatalf("route fixture parsed wrong: %+v", d)
	}

	// 兼容回归:无任何包裹的裸 JSON、带说明文字的 JSON 行为不变。
	if err := unmarshalLoose(`{"target":"a","args":{}}`, &d); err != nil || d.Target != "a" {
		t.Fatalf("bare JSON regressed: %v %+v", err, d)
	}
	if err := unmarshalLoose("选择如下:\n{\"target\":\"b\",\"args\":{}}\n以上。", &d); err != nil || d.Target != "b" {
		t.Fatalf("prose-wrapped JSON regressed: %v %+v", err, d)
	}
}
