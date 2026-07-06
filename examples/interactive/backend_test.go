package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestBackend 校验 mock 后端:数据规模、埋入的异常、改动性操作真实生效、
// 状态机校验。数据是确定性生成的,断言可以精确。
func TestBackend(t *testing.T) {
	srv := newBackendData().serve()
	defer srv.Close()

	get := func(path string) (int, map[string]any, []any) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]any
		var a []any
		if json.Unmarshal(body, &m) != nil {
			_ = json.Unmarshal(body, &a)
		}
		return resp.StatusCode, m, a
	}
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	// 规模:20 商品;品类过滤;下架商品 P111 可被状态过滤出来
	if _, _, all := get("/products"); len(all) != 20 {
		t.Fatalf("products = %d, want 20", len(all))
	}
	if _, _, audio := get("/products?category=音频"); len(audio) != 4 {
		t.Fatalf("audio = %d, want 4", len(audio))
	}
	if _, _, off := get("/products?status=下架"); len(off) != 1 {
		t.Fatalf("下架 = %d, want 1 (P111)", len(off))
	}

	// 异常:P115 亏本在售;P105 库存重复记录(4 行);P103 库存告急
	if _, p, _ := get("/products/P115"); p["price"].(float64) >= p["cost"].(float64) {
		t.Fatal("P115 must sell below cost")
	}
	if _, inv, _ := get("/inventory/P105"); len(inv["warehouses"].([]any)) != 4 {
		t.Fatal("P105 must have a duplicate stock row")
	}

	// 销售:P117 滞销 30 天 0 销量;P108 环比显著下滑;汇总含 trend_pct
	if _, s, _ := get("/sales/P117"); s["total_units"].(float64) != 0 {
		t.Fatal("P117 must be a slow mover with zero sales")
	}
	_, sum, _ := get("/sales-summary?days=30")
	declining := false
	for _, r := range sum["products"].([]any) {
		row := r.(map[string]any)
		if row["sku"] == "P108" && row["trend_pct"].(float64) < -30 {
			declining = true
		}
	}
	if !declining {
		t.Fatal("P108 must show a strong declining trend")
	}

	// 订单:总量 80;卡单 O-1042 已支付 12 天
	if _, list, _ := get("/orders?limit=5"); list["total"].(float64) != 80 || len(list["orders"].([]any)) != 5 {
		t.Fatalf("orders total/limit: %v", list)
	}
	if _, o, _ := get("/orders/O-1042"); o["status"] != "已支付" || o["days_ago"].(float64) != 12 {
		t.Fatalf("O-1042 stuck order: %v", o)
	}

	// 改动生效:改价后再查能看到;订单状态机拒绝非法流转、接受合法流转;退款
	if code, r := post("/price", `{"sku":"P115","price":"279"}`); code != 200 || r["new_price"].(float64) != 279 {
		t.Fatalf("price update: %d %v", code, r)
	}
	if _, p, _ := get("/products/P115"); p["price"].(float64) != 279 {
		t.Fatal("price change must persist")
	}
	if code, _ := post("/orders/status", `{"id":"O-1042","status":"已完成"}`); code != 400 {
		t.Fatal("已支付→已完成 must be rejected")
	}
	if code, _ := post("/orders/status", `{"id":"O-1042","status":"已发货"}`); code != 200 {
		t.Fatal("已支付→已发货 must pass")
	}
	if code, r := post("/orders/refund", `{"id":"O-1063","reason":"与描述不符"}`); code != 200 || r["status"] != "已退款" {
		t.Fatalf("refund O-1063: %d %v", code, r)
	}
	if code, _ := post("/orders/refund", `{"id":"O-1006","reason":"x"}`); code != 400 {
		t.Fatal("已取消订单不可退款") // k=5 → O-1006 已取消
	}

	// 库存调整生效
	if code, r := post("/stock", `{"sku":"P103","warehouse":"仓A","delta":100}`); code != 200 || r["available"].(float64) != 106 {
		t.Fatalf("restock P103: %d %v", code, r)
	}
}
