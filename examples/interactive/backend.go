// backend.go:内置 mock 业务后端(商品/库存/销售/订单/客户)。数据确定性
// 生成(无随机,重启一致),状态可变(改价/调库存/订单流转/退款真实生效,
// 后续查询能看到变化)。刻意埋入的异常,供复杂场景考察 agent:
//
//	P103 热销但库存告急(补货候选)     P108 销量持续下滑且库存积压(清仓候选)
//	P115 成本高于售价(亏本在售)       P117 连续 30 天零销量(滞销)
//	P111 已下架但仍有历史订单          P105 库存表有重复记录(数据质量)
//	O-1042 已付款 12 天未发货(卡单)   O-1063 已完成但客户申请退款
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type product struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Price    float64 `json:"price"`
	Cost     float64 `json:"cost"`
	Status   string  `json:"status"` // 在售 | 下架 | 预售
}

type stockRow struct {
	Warehouse string `json:"warehouse"`
	Available int    `json:"available"`
	Reserved  int    `json:"reserved"`
}

type order struct {
	ID       string  `json:"id"`
	Customer string  `json:"customer"`
	SKU      string  `json:"sku"`
	Qty      int     `json:"qty"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"` // 待支付|已支付|已发货|已完成|已取消|已退款
	DaysAgo  int     `json:"days_ago"`
	Note     string  `json:"note,omitempty"`
}

type customer struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Tier   string  `json:"tier"` // 黑卡 | VIP | 普通
	LTV    float64 `json:"ltv"`
	Note   string  `json:"note"`
	Orders int     `json:"orders"`
}

type backend struct {
	mu        sync.Mutex
	products  []*product
	stock     map[string][]*stockRow
	orders    []*order
	customers map[string]*customer
}

// newBackendData 生成确定性数据集:20 商品 × 6 品类 × 3 仓库 × 30 天销售
// × 80 订单 × 10 客户。
func newBackendData() *backend {
	b := &backend{stock: map[string][]*stockRow{}, customers: map[string]*customer{}}
	names := []struct {
		name, cat   string
		price, cost float64
	}{
		{"降噪耳机", "音频", 129, 80}, {"蓝牙音箱", "音频", 219, 130}, {"入耳式耳机", "音频", 79, 42}, {"头戴电竞耳机", "音频", 349, 210},
		{"机械键盘", "键鼠外设", 399, 240}, {"无线鼠标", "键鼠外设", 149, 88}, {"键鼠套装", "键鼠外设", 279, 170}, {"电竞鼠标垫", "键鼠外设", 59, 22},
		{"27寸显示器", "显示器", 899, 620}, {"32寸带鱼屏", "显示器", 1899, 1350}, {"便携副屏", "显示器", 699, 460},
		{"512G固态硬盘", "存储", 329, 215}, {"1T移动硬盘", "存储", 419, 290}, {"256G内存卡", "存储", 119, 68},
		{"千兆路由器", "网络设备", 169, 105}, {"路由器Pro", "网络设备", 199, 230}, {"网络交换机", "网络设备", 259, 165},
		{"智能灯泡", "智能家居", 49, 21}, {"智能插座", "智能家居", 69, 34}, {"温湿度传感器", "智能家居", 89, 47},
	}
	for i, n := range names {
		p := &product{ID: fmt.Sprintf("P%d", 100+i), Name: n.name, Category: n.cat, Price: n.price, Cost: n.cost, Status: "在售"}
		if p.ID == "P111" {
			p.Status = "下架" // 异常:下架但有历史订单
		}
		b.products = append(b.products, p)
		base := (i*37)%160 + 20
		rows := []*stockRow{
			{Warehouse: "仓A", Available: base, Reserved: base / 10},
			{Warehouse: "仓B", Available: base * 2 / 3, Reserved: base / 15},
			{Warehouse: "仓C", Available: base / 2, Reserved: 0},
		}
		switch p.ID {
		case "P103": // 热销库存告急
			rows = []*stockRow{{Warehouse: "仓A", Available: 6, Reserved: 4}, {Warehouse: "仓B", Available: 3, Reserved: 2}, {Warehouse: "仓C", Available: 0, Reserved: 0}}
		case "P108": // 积压
			rows = []*stockRow{{Warehouse: "仓A", Available: 460, Reserved: 2}, {Warehouse: "仓B", Available: 380, Reserved: 0}, {Warehouse: "仓C", Available: 290, Reserved: 0}}
		case "P105": // 重复记录(数据质量异常)
			rows = append(rows, &stockRow{Warehouse: "仓A", Available: base, Reserved: base / 10})
		}
		b.stock[p.ID] = rows
	}
	tiers := []string{"黑卡", "VIP", "普通", "普通", "VIP", "普通", "黑卡", "普通", "VIP", "普通"}
	cnames := []string{"陈晨", "李雷", "韩梅", "王芳", "赵磊", "孙悦", "周涛", "吴倩", "郑爽", "钱多"}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("C%d", i+1)
		b.customers[id] = &customer{ID: id, Name: cnames[i], Tier: tiers[i], LTV: float64(800 + i*730), Note: "近期活跃"}
	}
	b.customers["C3"].Note = "多次咨询售后,对物流时效敏感"
	statuses := []string{"已完成", "已完成", "已发货", "已支付", "已完成", "已取消", "已完成", "已退款"}
	for k := 0; k < 80; k++ {
		p := b.products[(k*13)%20]
		qty := k%3 + 1
		o := &order{
			ID: fmt.Sprintf("O-%d", 1001+k), Customer: fmt.Sprintf("C%d", k%10+1),
			SKU: p.ID, Qty: qty, Amount: p.Price * float64(qty),
			Status: statuses[k%len(statuses)], DaysAgo: (k*7)%30 + 1,
		}
		switch o.ID {
		case "O-1042":
			o.Status, o.DaysAgo, o.Note = "已支付", 12, "客户催促发货两次"
		case "O-1063":
			o.Status, o.Note = "已完成", "客户申请退款:称与描述不符"
		}
		b.orders = append(b.orders, o)
		b.customers[o.Customer].Orders++
	}
	return b
}

// unitsSold 是确定性销售曲线:d 为几天前(1=昨天 … 30)。埋入趋势异常。
func (b *backend) unitsSold(skuIdx, d int) int {
	p := b.products[skuIdx]
	base := (skuIdx*7)%23 + 3
	u := base
	if d%7 < 2 {
		u = u * 3 / 2 // 周末小高峰
	}
	switch p.ID {
	case "P103":
		u += (30 - d) * 2 // 持续上升,越近越高
	case "P108":
		u = base * d / 22 // 持续下滑,越近越低
	case "P117":
		u = 0 // 滞销
	case "P111":
		if d < 10 {
			u = 0 // 10 天前下架
		}
	}
	if u < 0 {
		u = 0
	}
	return u
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (b *backend) findProduct(id string) (*product, int) {
	for i, p := range b.products {
		if p.ID == id {
			return p, i
		}
	}
	return nil, -1
}

// serve 启动 mock 后端。
func (b *backend) serve() *httptest.Server {
	mux := http.NewServeMux()

	// ---- 商品 ----
	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		q, cat, st := r.URL.Query().Get("q"), r.URL.Query().Get("category"), r.URL.Query().Get("status")
		var out []*product
		for _, p := range b.products {
			if q != "" && !strings.Contains(p.Name, q) && !strings.Contains(p.Category, q) && !strings.Contains(p.ID, q) {
				continue
			}
			if cat != "" && p.Category != cat {
				continue
			}
			if st != "" && p.Status != st {
				continue
			}
			out = append(out, p)
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/products/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/products/")
		if p, _ := b.findProduct(id); p != nil {
			writeJSON(w, p)
			return
		}
		http.Error(w, `{"error":"商品不存在"}`, 404)
	})
	mux.HandleFunc("/price", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var in struct {
			SKU   string `json:"sku"`
			Price any    `json:"price"` // 字符串或数字都接受
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"参数格式错误"}`, 400)
			return
		}
		var newPrice float64
		switch v := in.Price.(type) {
		case float64:
			newPrice = v
		case string:
			if _, err := fmt.Sscanf(v, "%f", &newPrice); err != nil {
				http.Error(w, `{"error":"price 不是数字"}`, 400)
				return
			}
		default:
			http.Error(w, `{"error":"缺少 price"}`, 400)
			return
		}
		p, _ := b.findProduct(in.SKU)
		if p == nil {
			http.Error(w, `{"error":"商品不存在"}`, 404)
			return
		}
		old := p.Price
		p.Price = newPrice
		writeJSON(w, map[string]any{"ok": true, "sku": p.ID, "old_price": old, "new_price": p.Price})
	})
	mux.HandleFunc("/products/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var in struct{ SKU, Status string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		p, _ := b.findProduct(in.SKU)
		if p == nil {
			http.Error(w, `{"error":"商品不存在"}`, 404)
			return
		}
		if in.Status != "在售" && in.Status != "下架" && in.Status != "预售" {
			http.Error(w, `{"error":"status 只能是 在售|下架|预售"}`, 400)
			return
		}
		p.Status = in.Status
		writeJSON(w, map[string]any{"ok": true, "sku": p.ID, "status": p.Status})
	})

	// ---- 库存 ----
	mux.HandleFunc("/inventory/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		sku := strings.TrimPrefix(r.URL.Path, "/inventory/")
		rows, ok := b.stock[sku]
		if !ok {
			http.Error(w, `{"error":"SKU 不存在"}`, 404)
			return
		}
		_, idx := b.findProduct(sku)
		// 附 30 天出入库流水(数据量大,考察 digest)
		var moves []map[string]any
		for d := 30; d >= 1; d-- {
			out := b.unitsSold(idx, d)
			moves = append(moves, map[string]any{"days_ago": d, "outbound": out, "inbound": (out / 3) * 3})
		}
		writeJSON(w, map[string]any{"sku": sku, "warehouses": rows, "movements_30d": moves})
	})
	mux.HandleFunc("/stock", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var in struct {
			SKU, Warehouse string
			Delta          int
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, row := range b.stock[in.SKU] {
			if row.Warehouse == in.Warehouse {
				row.Available += in.Delta
				writeJSON(w, map[string]any{"ok": true, "sku": in.SKU, "warehouse": in.Warehouse, "available": row.Available})
				return
			}
		}
		http.Error(w, `{"error":"SKU 或仓库不存在"}`, 404)
	})

	// ---- 销售 ----
	mux.HandleFunc("/sales/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		sku := strings.TrimPrefix(r.URL.Path, "/sales/")
		p, idx := b.findProduct(sku)
		if p == nil {
			http.Error(w, `{"error":"SKU 不存在"}`, 404)
			return
		}
		days := 30
		fmt.Sscanf(r.URL.Query().Get("days"), "%d", &days)
		if days < 1 || days > 30 {
			days = 30
		}
		var series []map[string]any
		total, revenue := 0, 0.0
		for d := days; d >= 1; d-- {
			u := b.unitsSold(idx, d)
			total += u
			revenue += float64(u) * p.Price
			series = append(series, map[string]any{"days_ago": d, "units": u, "revenue": float64(u) * p.Price})
		}
		writeJSON(w, map[string]any{"sku": sku, "name": p.Name, "days": days,
			"total_units": total, "total_revenue": revenue, "daily": series})
	})
	mux.HandleFunc("/sales-summary", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		days := 30
		fmt.Sscanf(r.URL.Query().Get("days"), "%d", &days)
		if days < 1 || days > 30 {
			days = 30
		}
		cat := r.URL.Query().Get("category")
		type rowT struct {
			SKU     string  `json:"sku"`
			Name    string  `json:"name"`
			Cat     string  `json:"category"`
			Units   int     `json:"units"`
			Revenue float64 `json:"revenue"`
			// 环比:后半段对前半段的销量变化率(%),负数=下滑
			TrendPct int `json:"trend_pct"`
		}
		var rows []rowT
		for i, p := range b.products {
			if cat != "" && p.Category != cat {
				continue
			}
			units, older, newer := 0, 0, 0
			for d := days; d >= 1; d-- {
				u := b.unitsSold(i, d)
				units += u
				if d > days/2 {
					older += u
				} else {
					newer += u
				}
			}
			trend := 0
			if older > 0 {
				trend = (newer - older) * 100 / older
			}
			rows = append(rows, rowT{SKU: p.ID, Name: p.Name, Cat: p.Category,
				Units: units, Revenue: float64(units) * p.Price, TrendPct: trend})
		}
		writeJSON(w, map[string]any{"days": days, "products": rows})
	})

	// ---- 订单 ----
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		st, cust, sku := r.URL.Query().Get("status"), r.URL.Query().Get("customer"), r.URL.Query().Get("sku")
		limit := 20
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		var out []*order
		matched := 0
		for _, o := range b.orders {
			if st != "" && o.Status != st {
				continue
			}
			if cust != "" && o.Customer != cust {
				continue
			}
			if sku != "" && o.SKU != sku {
				continue
			}
			matched++
			if len(out) < limit {
				out = append(out, o)
			}
		}
		writeJSON(w, map[string]any{"total": matched, "returned": len(out), "orders": out})
	})
	mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/orders/")
		for _, o := range b.orders {
			if o.ID == id {
				writeJSON(w, o)
				return
			}
		}
		http.Error(w, `{"error":"订单不存在"}`, 404)
	})
	mux.HandleFunc("/orders/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var in struct{ ID, Status string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		valid := map[string][]string{ // 合法流转
			"待支付": {"已支付", "已取消"}, "已支付": {"已发货", "已取消"}, "已发货": {"已完成"},
		}
		for _, o := range b.orders {
			if o.ID != in.ID {
				continue
			}
			for _, next := range valid[o.Status] {
				if next == in.Status {
					o.Status = in.Status
					writeJSON(w, map[string]any{"ok": true, "id": o.ID, "status": o.Status})
					return
				}
			}
			http.Error(w, fmt.Sprintf(`{"error":"订单状态 %s 不能流转到 %s"}`, o.Status, in.Status), 400)
			return
		}
		http.Error(w, `{"error":"订单不存在"}`, 404)
	})
	mux.HandleFunc("/orders/refund", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var in struct{ ID, Reason string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, o := range b.orders {
			if o.ID != in.ID {
				continue
			}
			switch o.Status {
			case "已支付", "已发货", "已完成":
				o.Status = "已退款"
				o.Note = "退款原因:" + in.Reason
				writeJSON(w, map[string]any{"ok": true, "id": o.ID, "refunded": o.Amount, "status": o.Status})
			default:
				http.Error(w, fmt.Sprintf(`{"error":"状态 %s 的订单不可退款"}`, o.Status), 400)
			}
			return
		}
		http.Error(w, `{"error":"订单不存在"}`, 404)
	})

	// ---- 客户 ----
	mux.HandleFunc("/customers/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/customers/")
		if c, ok := b.customers[id]; ok {
			writeJSON(w, c)
			return
		}
		http.Error(w, `{"error":"客户不存在"}`, 404)
	})

	return httptest.NewServer(mux)
}
