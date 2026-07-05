package config

// TestLiveSmoke:真实 MiniMax 模型驱动的全特性冒烟矩阵。
//
// 设计原则(live 测试的纪律):
//   - 分层断言:机制事实硬断言(工具被调、store 落盘、错误类型、JSON 可解析、
//     审批被触达),模型措辞软断言(非空 + 关键实体,失配只告警不判死);
//   - 每个子测试独立会话、独立数据目录,串行执行(尊重限流,失败可定位);
//   - 后端是本地 mock(httptest),只有模型是真的——断言"框架行为",
//     不断言"业务正确";
//   - 成本有界:每子测试 1-4 轮对话,整套 ~30 次模型调用。
//
// 运行方式(key 只经环境变量,不落仓库与日志):
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	SMOKE_LIVE=1 go test ./config/ -run TestLiveSmoke -v -count=1 -timeout 30m
//
// 覆盖矩阵(→ 子测试):react 工具循环→01;会话记忆→02;plan-execute/graph
// 编排 + digest→03;todo 纪律→04;长期记忆 + 召回→05;审批(interactive +
// 参数级 allow 规则 + deny 规则)→06/07;预算硬停→08;结构化输出→09;
// 上下文压缩→10;窗外会话召回→11;HTTP gateway + A2A→12;挂起/恢复
// (dispatcher + 假通道 + file KV)→13;副本重启(file session)→14;
// exectool 脚本执行→15;中断→16;轨迹落盘→17(随主 app 顺带断言)。

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/serving"

	_ "github.com/joewm9911/agent-kit/impl/source/exectool"
)

// liveInteractor:ask_user 回脚本答案、审批放行,全程计数(硬断言用)。
type liveInteractor struct {
	asks     atomic.Int32
	approves atomic.Int32
}

func (ix *liveInteractor) Ask(context.Context, string) (string, error) {
	ix.asks.Add(1)
	return "预算 5 万元,下周一上线。", nil
}
func (ix *liveInteractor) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	ix.approves.Add(1)
	return true, nil
}

// liveEnv 装配真实模型环境:mock 业务后端 + minimax provider。
func liveEnv(t *testing.T, dataDir string) *smokeBackend {
	t.Helper()
	backend, srv := newSmokeBackend(t)
	t.Setenv("SMOKE_MODEL_PROVIDER", "minimax")
	if os.Getenv("SMOKE_MODEL_BASE") == "" {
		t.Setenv("SMOKE_MODEL_BASE", "https://api.minimaxi.com/v1") // 国内平台;海外换 api.minimax.io
	}
	t.Setenv("SMOKE_API_BASE", srv.URL)
	t.Setenv("SMOKE_DATA_DIR", dataDir)
	return backend
}

// liveApp 加载 smoke 配置树、应用 mutate 覆盖后装配。
func liveApp(t *testing.T, ix runctx.Interactor, mutate func(*AppSpec)) *App {
	t.Helper()
	spec, err := LoadApp("../examples/smoke/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(spec)
	}
	app, err := BuildApp(context.Background(), spec, BuildOptions{Interactor: ix})
	if err != nil {
		t.Fatalf("build live app: %v", err)
	}
	return app
}

// run 执行一轮并做通用硬断言(不报错、非空),返回回答。
// 厂商瞬时 5xx(框架内已重试)整轮再兜一次:live 冒烟断言框架行为,
// 不断言厂商 SLA;非瞬时错误照常判死。
func run(t *testing.T, a *agent.Agent, sess, input string) string {
	t.Helper()
	ctx := runctx.WithInput(runctx.WithUser(context.Background(), "u-live"), input)
	start := time.Now()
	out, err := a.Run(ctx, sess, input)
	for attempt, wait := 0, 15*time.Second; attempt < 2; attempt, wait = attempt+1, wait*2 {
		if !(err != nil && loop.Transient(err) || err == nil && strings.TrimSpace(out) == "") {
			break
		}
		// 厂商瞬时故障有粘性窗口(观测 ~30s),间隔拉长跨过去再整轮重试。
		t.Logf("⚠ 瞬时厂商异常(err=%v, empty=%v),%s 后重试整轮", err, strings.TrimSpace(out) == "", wait)
		time.Sleep(wait)
		out, err = a.Run(ctx, sess, input)
	}
	if err != nil && loop.Transient(err) {
		// 框架内 3 次重试 + 测试级 2 次整轮重试后仍 5xx:厂商分钟级故障,
		// 结论是"本子测试不确定",不是框架失败——skip 留证,别的子测试继续。
		t.Skipf("厂商持续瞬时故障(框架重试已尽),本项不确定: %v", err)
	}
	if err != nil {
		t.Fatalf("run(%s) error: %v", sess, err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("run(%s) empty answer", sess)
	}
	t.Logf("[%s] %.1fs\nQ: %s\nA: %s", sess, time.Since(start).Seconds(), input, truncate(out, 280))
	return out
}

// softContains:模型措辞软断言——失配只告警,不判死。
func softContains(t *testing.T, out, want, feature string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Logf("⚠ 软断言未命中(%s):回答未包含 %q(模型措辞自由度,人工复核)", feature, want)
	}
}

func TestLiveSmoke(t *testing.T) {
	if os.Getenv("SMOKE_LIVE") == "" || os.Getenv("MINIMAX_API_KEY") == "" {
		t.Skip("set SMOKE_LIVE=1 and MINIMAX_API_KEY to run the live smoke")
	}
	registerRecordingKV()
	dataDir := t.TempDir()
	backend := liveEnv(t, dataDir)
	ix := &liveInteractor{}
	trajPath := dataDir + "/trajectory.jsonl"

	// 主 app:todo/result 换记录型后端(硬断言落盘),开轨迹,开 gateway。
	app := liveApp(t, ix, func(spec *AppSpec) {
		spec.App.Serving.Addr = "127.0.0.1:0"
		spec.App.Observability.TrajectoryPath = trajPath
		for _, as := range spec.Agents {
			if as.Name != "ops-manager" {
				continue
			}
			for i, si := range as.Stores {
				if si.Kind == "todo" || si.Kind == "result" {
					as.Stores[i].Type = "reckv"
					as.Stores[i].Config = map[string]any{"name": "live-" + si.Name}
				}
			}
		}
	})
	ops := app.Agents["ops-manager"]
	support := app.Agents["support-bot"]
	ctxBg := context.Background()

	// —— 01 react 工具循环:模型必须真的调 quick-product-qa 到 mock 后端 ——
	t.Run("01_ReactToolLoop", func(t *testing.T) {
		before := backend.searches.Load()
		out := run(t, ops, "live-01", "用 quick-product-qa 查一下降噪耳机现在卖什么价,报给我具体数字。")
		if backend.searches.Load() == before {
			t.Fatal("模型未触达商品搜索后端(工具循环未走通)")
		}
		softContains(t, out, "129", "转述后端价格")
	})

	// —— 02 会话记忆:第二轮引用第一轮实体 ——
	t.Run("02_SessionMemory", func(t *testing.T) {
		run(t, ops, "live-02", "用 quick-product-qa 查降噪耳机的价格。")
		out := run(t, ops, "live-02", "我刚才让你查的是什么商品?只说商品名。")
		if !strings.Contains(out, "耳机") {
			t.Fatalf("跨轮记忆失效,第二轮不知道第一轮查了什么: %q", out)
		}
		if ents, err := os.ReadDir(dataDir + "/ops-sessions"); err != nil || len(ents) == 0 {
			t.Fatalf("会话历史未落 file 后端: %v", err)
		}
	})

	// —— 03 编排 + digest:price-review(graph 编排)穿库存大结果,消化落暂存 ——
	t.Run("03_OrchestrationAndDigest", func(t *testing.T) {
		before := backend.inventory.Load()
		run(t, ops, "live-03", "用 price-review 给 P100 做一次完整定价审查,库存、成本、建议价都要覆盖。")
		if backend.inventory.Load() == before {
			t.Fatal("编排未触达库存后端(price-review 流程未走通)")
		}
		// 库存响应 ~4800 字 > digest.over 4000:全文必须进结果暂存
		keys, _ := recordedKV(t, "live-cache").Scan(ctxBg, "")
		if len(keys) == 0 {
			t.Fatal("大结果未进 digest 暂存(digest 管线未触发)")
		}
	})

	// —— 04 todo 纪律:多步任务先外化计划 ——
	t.Run("04_TodoDiscipline", func(t *testing.T) {
		run(t, ops, "live-04", "三步任务:先查 P100 价格,再查 C1 客户情况,最后汇总。先用 todo_write 列出计划再逐步执行。")
		keys, _ := recordedKV(t, "live-plans").Scan(ctxBg, "")
		if len(keys) == 0 {
			t.Fatal("todo_write 未落计划后端(计划外化纪律未生效)")
		}
	})

	// —— 05 长期记忆:memory_save 落库,新会话经自动召回读回 ——
	t.Run("05_LongTermMemory", func(t *testing.T) {
		run(t, ops, "live-05a", "请用 memory_save 记住:我的汇报偏好是「喜欢简短汇报,先说结论」。")
		out := run(t, ops, "live-05b", "按照我的汇报偏好,现在应该怎么向我汇报?")
		softContains(t, out, "简短", "跨会话长期记忆召回")
	})

	// —— 06 审批:正常调价必须走 Approve,灰度 SKU 命中 allow 规则免批 ——
	t.Run("06_ApprovalGateAndAllowRule", func(t *testing.T) {
		beforeApprove, beforePrice := ix.approves.Load(), backend.priceHits.Load()
		run(t, ops, "live-06", "用 apply-price 把 P100 的价格正式调整为 199 元。")
		if backend.priceHits.Load() == beforePrice {
			t.Fatal("调价未触达后端")
		}
		if ix.approves.Load() == beforeApprove {
			t.Fatal("mutating 调价未经审批(Ring 0 审批闸门失效)")
		}
		beforeApprove, beforePrice = ix.approves.Load(), backend.priceHits.Load()
		run(t, ops, "live-06", "再用 apply-price 把灰度 SKU CANARY-1 调到 9.9 元。")
		if backend.priceHits.Load() == beforePrice {
			t.Fatal("灰度调价未触达后端")
		}
		if ix.approves.Load() != beforeApprove {
			t.Fatal("CANARY-* 命中 allow 规则仍走了审批(参数级策略失效)")
		}
	})

	// —— 07 审批 deny 规则:后端必须不被触达 ——
	t.Run("07_ApprovalDenyRule", func(t *testing.T) {
		denyApp := liveApp(t, ix, func(spec *AppSpec) {
			for _, as := range spec.Agents {
				if as.Name == "ops-manager" {
					as.Approval.Rules = []loop.ApprovalRule{
						{Ref: "cap://skill/catalog/apply-price", Action: "deny"},
					}
				}
			}
		})
		before := backend.priceHits.Load()
		out := run(t, denyApp.Agents["ops-manager"], "live-07", "用 apply-price 把 P100 调价到 99 元。")
		if backend.priceHits.Load() != before {
			t.Fatal("deny 规则下调价仍触达了后端(治理失效)")
		}
		softContains(t, out, "拒", "模型解释被拒")
	})

	// —— 08 预算硬停:support-bot max_model_calls=4,连环任务必须撞墙 ——
	t.Run("08_BudgetHardStop", func(t *testing.T) {
		ctx := runctx.WithInput(ctxBg, "budget")
		_, err := support.Run(ctx, "live-08",
			"依次用 quick-product-qa 查询这 5 个商品的价格,一个一个查:降噪耳机、键盘、鼠标、显示器、音箱。全部查完再汇总。")
		if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
			t.Fatalf("预算硬停未生效,want ErrBudgetExhausted, got %v", err)
		}
	})

	// —— 09 结构化输出:最终回答必须是合规 JSON ——
	t.Run("09_StructuredOutput", func(t *testing.T) {
		structApp := liveApp(t, ix, func(spec *AppSpec) {
			for _, as := range spec.Agents {
				if as.Name == "ops-manager" {
					as.StructuredOutput = loop.StructuredConfig{
						Schema: `{"type":"object","required":["product","price"],
							"properties":{"product":{"type":"string"},"price":{"type":"number"}}}`,
						MaxRetries: 2,
					}
				}
			}
		})
		out := run(t, structApp.Agents["ops-manager"], "live-09", "查一下降噪耳机的价格,按要求的结构输出。")
		var parsed struct {
			Product string  `json:"product"`
			Price   float64 `json:"price"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("结构化输出不是合法 JSON: %v\n%s", err, out)
		}
		if parsed.Product == "" || parsed.Price == 0 {
			t.Fatalf("结构化输出缺必填字段: %+v", parsed)
		}
	})

	// —— 10 上下文压缩:压低阈值,多轮后必须触发滚动摘要 ——
	t.Run("10_Compaction", func(t *testing.T) {
		var compactions int32
		old := slog.Default()
		slog.SetDefault(slog.New(countHandler{substr: "compacted", n: &compactions}))
		defer slog.SetDefault(old)

		compApp := liveApp(t, ix, func(spec *AppSpec) {
			for _, as := range spec.Agents {
				if as.Name == "ops-manager" {
					as.Loop.Compaction = &loop.CompactionConfig{MaxMessages: 6, KeepRecent: 2}
				}
			}
		})
		a := compApp.Agents["ops-manager"]
		for _, q := range []string{
			"用 quick-product-qa 查降噪耳机价格。",
			"查询客户 C1 的情况(customer-brief)。",
			"把上面两件事各用一句话总结。",
			"好的,收到。请再确认一遍商品名。",
		} {
			run(t, a, "live-10", q)
		}
		a.WaitCompactions()
		if atomic.LoadInt32(&compactions) == 0 {
			t.Fatal("多轮对话未触发上下文压缩(compaction 未生效)")
		}
	})

	// —— 11 窗外召回:实体滑出窗口后仍可经 bigram 召回 ——
	t.Run("11_RecallBeyondWindow", func(t *testing.T) {
		recallApp := liveApp(t, ix, func(spec *AppSpec) {
			for _, as := range spec.Agents {
				if as.Name == "ops-manager" {
					as.Session.Window = 4 // 极小窗口:第一轮必然滑出
					as.Loop.Compaction = nil
				}
			}
		})
		a := recallApp.Agents["ops-manager"]
		run(t, a, "live-11", "记一下:我们这次大促的内部项目代号是「青鸟计划」。")
		run(t, a, "live-11", "用 quick-product-qa 查降噪耳机价格。")
		run(t, a, "live-11", "查询客户 C1 的情况。")
		out := run(t, a, "live-11", "我们这次大促的内部项目代号是什么?")
		if !strings.Contains(out, "青鸟") {
			t.Fatalf("窗外召回失效:代号在窗口外且未被召回, got %q", out)
		}
	})

	// —— 12 HTTP gateway + A2A 供给面 ——
	t.Run("12_GatewayAndA2A", func(t *testing.T) {
		srv := httptest.NewServer(app.Server.Mux())
		defer srv.Close()

		if resp, err := http.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz: %v %v", resp, err)
		}
		resp, err := http.Get(srv.URL + "/a2a/agents")
		if err != nil {
			t.Fatal(err)
		}
		var list []struct{ Name string }
		_ = json.NewDecoder(resp.Body).Decode(&list)
		if len(list) != 2 {
			t.Fatalf("A2A 目录应有 2 个 agent, got %v", list)
		}
		body := strings.NewReader(`{"session":"live-12","input":"用 quick-product-qa 查降噪耳机价格","user":"u-live"}`)
		resp, err = http.Post(srv.URL+"/agents/ops-manager/messages", "application/json", body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("messages: %v %v", resp.Status, err)
		}
		var ans map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&ans)
		if strings.TrimSpace(ans["answer"]) == "" {
			t.Fatalf("gateway 空回答: %v", ans)
		}
		t.Logf("[gateway] A: %s", truncate(ans["answer"], 200))
	})

	// —— 13 挂起/恢复:dispatcher + 假通道 + file KV,跨"进程"续跑 ——
	t.Run("13_SuspendResume", func(t *testing.T) {
		pendKV, err := store.NewBackend("file", map[string]any{"dir": dataDir + "/pending"})
		if err != nil {
			t.Fatal(err)
		}
		fc := &fakeChannel{sends: make(chan string, 8)}
		d := serving.NewDispatcher(nil)
		d.EnableSuspend(pendKV)
		h := d.Handler(serving.Binding{Channel: fc, Agent: ops, SessionMapping: "chat"})

		conv := channel.ConvRef{Channel: "fake", Chat: "c-13", User: "u-live"}
		h(ctxBg, channel.Inbound{Conv: conv, Text: "帮我策划下周一上线的降噪耳机私域大促活动,先分步骤规划再写一版文案。预算等缺失信息用 ask_user 工具问我,别自己猜,也别在回答文本里反问。", EventID: "e1"})

		first := waitSend(t, fc, 180*time.Second)
		pend, _ := pendKV.Scan(ctxBg, "turn\x1f")
		if len(pend) == 0 {
			t.Skipf("模型未走 ask_user 挂起路径(直接回答了),人工复核: %s", truncate(first, 120))
		}
		// 用户答复到达 → 命中挂起轮次 → 交互日志记答案 → 原输入重放续跑
		h(ctxBg, channel.Inbound{Conv: conv, Text: "预算 5 万元。", EventID: "e2"})
		final := waitSend(t, fc, 180*time.Second)
		if strings.TrimSpace(final) == "" {
			t.Fatal("恢复后未产出最终回答")
		}
		if pend, _ = pendKV.Scan(ctxBg, "turn\x1f"); len(pend) != 0 {
			t.Fatal("恢复完成后挂起轮次未清理")
		}
		t.Logf("[suspend] question: %s\nfinal: %s", truncate(first, 150), truncate(final, 200))
	})

	// —— 14 副本重启:全新 App 实例接同一 file 会话目录,续同一会话 ——
	t.Run("14_ReplicaRestartFileSession", func(t *testing.T) {
		app2 := liveApp(t, ix, nil)
		out := run(t, app2.Agents["ops-manager"], "live-02", "重启后确认:我们之前查过什么商品?")
		if !strings.Contains(out, "耳机") {
			t.Fatalf("副本重启后会话连续性丢失: %q", out)
		}
	})

	// —— 15 exectool:真实脚本执行(python3 门控)——
	t.Run("15_ExecTool", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not available")
		}
		cfg := &Config{
			Profile: Profile{Model: &ModelConfig{Provider: "minimax", Config: map[string]any{
				"api_key": os.Getenv("MINIMAX_API_KEY"), "base_url": os.Getenv("SMOKE_MODEL_BASE"),
			}}},
			// exec 工具 Risk=Dangerous:目录准入默认只到 mutating,必须显式提升
			// (这正是准入闸门的语义——脚本执行不允许被静默挂上工具面)。
			Catalog: CatalogConfig{MaxRisk: "dangerous"},
			Sources: []SourceConfig{{Name: "exec", Type: "exec", Config: map[string]any{
				"tools": []map[string]any{{"name": "python", "runtime": "python"}},
			}}},
			Agents: []AgentConfig{{
				Name:         "coder",
				Prompt:       PromptConfig{System: prompt.Value{Literal: "你是代码执行助手。所有计算必须通过调用 python 工具真实执行拿到输出,禁止自己在回答里手写代码或口算结果。"}},
				Capabilities: CapabilitiesConfig{Include: []string{"cap://tool/exec/python"}},
			}},
		}
		execApp, err := Build(ctxBg, cfg, BuildOptions{Interactor: ix})
		if err != nil {
			t.Fatal(err)
		}
		out := run(t, execApp.Agents["coder"], "live-15", "调用 python 工具执行脚本,计算 13 * 17 + 4,把脚本真实输出的数字报给我。")
		if !strings.Contains(out, "225") {
			t.Fatalf("脚本执行结果未回流(want 225): %q", out)
		}
	})

	// —— 16 中断:运行中的任务被 Interrupt 后必须尽快返回,不悬挂 ——
	t.Run("16_Interrupt", func(t *testing.T) {
		done := make(chan struct{})
		var out string
		var err error
		go func() {
			defer close(done)
			ctx := runctx.WithInput(ctxBg, "long")
			out, err = ops.Run(ctx, "live-16", "逐个用 quick-product-qa 查询 8 个商品:耳机、键盘、鼠标、显示器、音箱、摄像头、麦克风、支架。一个一个查。")
		}()
		time.Sleep(5 * time.Second) // 让首轮模型调用起跑
		ops.Interrupt("live-16")
		select {
		case <-done:
			t.Logf("[interrupt] err=%v answer=%s", err, truncate(out, 150))
		case <-time.After(120 * time.Second):
			t.Fatal("Interrupt 后运行悬挂未返回")
		}
	})

	// —— 17 轨迹落盘:整套跑完,trajectory JSONL 必须非空且逐行合法 ——
	t.Run("17_Trajectory", func(t *testing.T) {
		raw, err := os.ReadFile(trajPath)
		if err != nil || len(raw) == 0 {
			t.Fatalf("轨迹未落盘: %v", err)
		}
		first := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]
		var ev map[string]any
		if err := json.Unmarshal([]byte(first), &ev); err != nil {
			t.Fatalf("轨迹首行不是合法 JSON: %v", err)
		}
		t.Logf("[trajectory] %d bytes", len(raw))
	})
}

// fakeChannel:进程内假 IM 通道,捕获全部外发消息。
type fakeChannel struct {
	sends chan string
}

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Start(context.Context, *http.ServeMux, channel.InboundHandler) error {
	return nil
}
func (f *fakeChannel) Send(_ context.Context, _ channel.ConvRef, msg channel.Outbound) (string, error) {
	f.sends <- msg.Text
	return "m1", nil
}
func (f *fakeChannel) Update(context.Context, channel.ConvRef, string, channel.Outbound) error {
	return channel.ErrUpdateUnsupported
}

func waitSend(t *testing.T, fc *fakeChannel, timeout time.Duration) string {
	t.Helper()
	select {
	case s := <-fc.sends:
		return s
	case <-time.After(timeout):
		t.Fatal("等待通道外发消息超时")
		return ""
	}
}
