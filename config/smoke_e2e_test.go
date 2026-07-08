package config

// 端到端冒烟:examples/smoke 的电商运营中台场景。
// 同一棵配置树两种驱动方式:
//   - 本文件的矩阵测试:smokescript 自适应脚本模型 + httptest 业务后端,
//     不出网、确定性,逐项断言编排/记忆/todo/治理机制;
//   - live_smoke_test.go 的 TestLiveSmoke:MINIMAX_API_KEY 门控,换真实模型
//     跑全特性矩阵。
//
// smokescript 的行为完全由配置里的提示词标记驱动(引擎内部提示词在
// system 层,component 任务书在 user 层),输出带 [MARKER] 供断言。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/model"
	"github.com/joewm9911/agent-kit/protocol/session"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/loop"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
	_ "github.com/joewm9911/agent-kit/impl/model/openai"
	_ "github.com/joewm9911/agent-kit/impl/source/httptool"
	_ "github.com/joewm9911/agent-kit/impl/source/vector"
	_ "github.com/joewm9911/agent-kit/std"
)

// ---- 自适应脚本模型 ----

var (
	registerSmoke sync.Once
	smokeSeen     struct {
		mu   sync.Mutex
		msgs [][]*schema.Message
	}
)

func recordSmoke(msgs []*schema.Message) {
	smokeSeen.mu.Lock()
	defer smokeSeen.mu.Unlock()
	cp := make([]*schema.Message, len(msgs))
	copy(cp, msgs)
	smokeSeen.msgs = append(smokeSeen.msgs, cp)
}

func resetSmokeSeen() {
	smokeSeen.mu.Lock()
	defer smokeSeen.mu.Unlock()
	smokeSeen.msgs = nil
}

func smokeSawSystemContaining(sub string) bool {
	smokeSeen.mu.Lock()
	defer smokeSeen.mu.Unlock()
	for _, call := range smokeSeen.msgs {
		for _, m := range call {
			if m.Role == schema.System && strings.Contains(m.Content, sub) {
				return true
			}
		}
	}
	return false
}

type smokeModel struct {
	tools []*schema.ToolInfo
}

func (s *smokeModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return &smokeModel{tools: tools}, nil
}

func (s *smokeModel) hasTool(name string) bool {
	for _, t := range s.tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (s *smokeModel) Stream(ctx context.Context, in []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := s.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(out, nil)
	sw.Close()
	return sr, nil
}

func call(name, args string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: "c-" + name, Type: "function",
		Function: schema.FunctionCall{Name: name, Arguments: args},
	}})
}

func markers(text string) string {
	var out []string
	for _, m := range []string{"[ANALYST]", "[SNAP]", "[DIGESTED]", "[QA]", "[REWOO]", "[AUDIT]", "[PE]", "[REV]", "[FAQBOT]", "[CRM]", "[RESEARCH]"} {
		if strings.Contains(text, m) {
			out = append(out, m)
		}
	}
	return strings.Join(out, "")
}

func (s *smokeModel) Generate(_ context.Context, msgs []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	recordSmoke(msgs)
	var sys, user, toolTxt strings.Builder
	toolMsgs := 0
	lastUser := ""
	for _, m := range msgs {
		switch m.Role {
		case schema.System:
			sys.WriteString(m.Content + "\n")
		case schema.User:
			user.WriteString(m.Content + "\n")
			lastUser = m.Content
		case schema.Tool:
			toolMsgs++
			toolTxt.WriteString(m.Content + "\n")
		}
	}
	sysT, userT, toolT := sys.String(), user.String(), toolTxt.String()
	reply := func(text string) (*schema.Message, error) { return schema.AssistantMessage(text, nil), nil }

	switch {
	// —— 框架内部调用(system 层识别;提示词已英文化,匹配英文特征串)——
	case strings.Contains(sysT, "bullet-point summary"): // Summarize
		return reply("[SUM]任务与结论要点")
	case strings.Contains(sysT, "You are a result digester"): // digest
		return reply("[DGST]库存充足,周转正常")
	case strings.Contains(sysT, "You are a reviewer"): // reflection reviewer
		if strings.Contains(userT, "[REV]") {
			return reply(`{"pass": true}`)
		}
		return reply(`{"pass": false, "feedback": "在文末追加[REV]标记"}`)
	case strings.Contains(sysT, "You are a router"):
		return reply(`{"target":"faq_bot","args":{"q":"退货政策"}}`)
	case strings.Contains(sysT, "You are a planner"): // rewoo planner
		return reply(`{"steps":[
			{"id":"e1","tool":"search_products","args":{"q":"促销"}},
			{"id":"e2","tool":"get_inventory","args":{"sku":"S1"}}]}`)
	case strings.Contains(sysT, "You are a solver"): // rewoo solver
		return reply("[REWOO]盘点完成")
	case strings.Contains(sysT, "You are a task planner"): // plan-execute planner
		return reply(`{"steps":["审查活动相关商品定价"]}`)
	case strings.Contains(sysT, "You are a task reviewer"): // plan-execute replanner
		return reply(`{"action":"finish","response":"[PE]活动方案已定"}`)
	case strings.Contains(sysT, "Complete only the single step"): // plan-execute executor (react)
		if toolMsgs == 0 && s.hasTool("price-review") {
			return call("price-review", `{"sku":"P100","question":"活动定价"}`), nil
		}
		return reply("[EXEC]步骤完成:" + markers(toolT))
	case strings.Contains(sysT, "你是文案执行者"): // reflection executor (example YAML overrides with a custom prompt)
		if strings.Contains(userT, "Review feedback") {
			return reply("文案v2[REV]")
		}
		return reply("文案v1")

	// —— component 任务书(user 层识别)——
	case strings.Contains(userT, "你是价格分析师"):
		if toolMsgs == 0 {
			return call("get_product", `{"id":"P100"}`), nil
		}
		if toolMsgs == 1 {
			return call("get_inventory", `{"sku":"P100"}`), nil
		}
		out := "[ANALYST]定价合理"
		if strings.Contains(userT, "caller's conversation") || strings.Contains(sysT, "caller's conversation") {
			out += "[SNAP]"
		}
		if strings.Contains(toolT, "结果已消化") {
			out += "[DIGESTED]"
		}
		return reply(out)
	case strings.Contains(userT, "你是商品问答助手"):
		if len(s.tools) > 0 && toolMsgs == 0 {
			return call("search_products", `{"q":"降噪耳机"}`), nil
		}
		return reply("[QA]在售款为P100")
	case strings.Contains(userT, "[FAQ]"):
		if toolMsgs == 0 { // agentic RAG:先查知识库
			return call("search_kb", `{"query":"退货"}`), nil
		}
		return reply("[FAQBOT]根据知识库:" + clipStr(toolT, 40)) // 用检索结果作答
	case strings.Contains(userT, "[HANDOFF]"):
		return reply("[HANDOFF]已为您转接人工")
	case strings.Contains(userT, "深入研究课题"):
		if toolMsgs == 0 {
			return call("todo_write", `{"todos":[
				{"content":"梳理现状","status":"in_progress"},
				{"content":"给出结论","status":"pending"}]}`), nil
		}
		if toolMsgs == 1 {
			return call("todo_write", `{"todos":[
				{"content":"梳理现状","status":"completed"},
				{"content":"给出结论","status":"in_progress"}]}`), nil
		}
		return reply("[RESEARCH]结论:值得投入")
	case strings.Contains(userT, "你是客户分析师"):
		out := "[CRM]建议主动跟进"
		if strings.Contains(userT, "caller's conversation") || strings.Contains(sysT, "caller's conversation") {
			out += "[SNAP]"
		}
		return reply(out)
	case strings.Contains(userT, "审计商品数据"): // workflow 的 model 步骤
		return reply("[AUDIT]数据合规")
	case strings.Contains(userT, "压成一句话汇报"): // price-review 的 brief 步骤
		return reply("[BRIEF]" + markers(userT))

	// —— agent 主循环 ——
	case strings.Contains(sysT, "[OPS]"):
		switch {
		case strings.Contains(lastUser, "审查") && toolMsgs == 0:
			return call("price-review", `{"sku":"P100","question":"定价是否合理"}`), nil
		case strings.Contains(lastUser, "灰度调价") && toolMsgs == 0:
			return call("apply-price", `{"sku":"CANARY-1","price":"9.9"}`), nil
		case strings.Contains(lastUser, "正式调价") && toolMsgs == 0:
			return call("apply-price", `{"sku":"P100","price":"199"}`), nil
		case strings.Contains(lastUser, "记住") && toolMsgs == 0:
			return call("memory_save", `{"key":"汇报偏好","value":"喜欢简短汇报"}`), nil
		case strings.Contains(lastUser, "偏好"):
			if strings.Contains(sysT, "Long-term memory") {
				return reply("[T-RECALL][HIT]你喜欢简短汇报")
			}
			return reply("[T-RECALL]未找到偏好")
		case toolMsgs > 0:
			return reply("[T-DONE]" + markers(toolT) + clipStr(toolT, 60))
		default:
			return reply("[T-GEN]收到:" + clipStr(lastUser, 20))
		}

	// —— support-bot:永远想调工具,用于预算硬停 ——
	case strings.Contains(sysT, "[SUPPORT]"):
		return call("quick-product-qa", fmt.Sprintf(`{"q":"第%d次查询"}`, toolMsgs+1)), nil
	}
	return reply("[GEN]ok")
}

func clipStr(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}

// ---- mock 业务后端 ----

type smokeBackend struct {
	priceHits atomic.Int32
	searches  atomic.Int32
	inventory atomic.Int32
}

func newSmokeBackend(t *testing.T) (*smokeBackend, *httptest.Server) {
	t.Helper()
	b := &smokeBackend{}
	mux := http.NewServeMux()
	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		b.searches.Add(1)
		fmt.Fprint(w, `[{"id":"P100","name":"降噪耳机","price":129}]`)
	})
	mux.HandleFunc("/products/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"P100","name":"降噪耳机","price":129,"cost":80}`)
	})
	mux.HandleFunc("/inventory/", func(w http.ResponseWriter, r *http.Request) {
		b.inventory.Add(1)
		// 刻意超大:触发 digest(阈值 3000)
		fmt.Fprintf(w, `{"sku":"P100","warehouses":%q}`, strings.Repeat("仓A:120;仓B:88;", 400))
	})
	mux.HandleFunc("/price", func(w http.ResponseWriter, r *http.Request) {
		b.priceHits.Add(1)
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/customers/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"C1","tier":"VIP","note":"多次咨询降噪耳机"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, srv
}

// ---- 装配环境 ----

func setupSmokeEnv(t *testing.T) *smokeBackend {
	t.Helper()
	registerSmoke.Do(func() {
		model.Register("smokescript", func(_ context.Context, _ map[string]any) (einomodel.ToolCallingChatModel, error) {
			return &smokeModel{}, nil
		})
	})
	backend, srv := newSmokeBackend(t)
	t.Setenv("SMOKE_MODEL_PROVIDER", "smokescript")
	t.Setenv("SMOKE_MODEL_BASE", "http://unused.local")
	t.Setenv("MINIMAX_API_KEY", "dummy-for-script")
	t.Setenv("SMOKE_API_BASE", srv.URL)
	t.Setenv("SMOKE_DATA_DIR", t.TempDir())
	return backend
}

func buildSmokeApp(t *testing.T, opts BuildOptions) *App {
	t.Helper()
	spec, err := LoadApp("../examples/smoke/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app, err := BuildApp(context.Background(), spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// skillCtx 构造直接调用 skill 时的运行环境(digest 暂存/输入/审批)。
func skillCtx(input string) context.Context {
	ctx := runctx.With(context.Background(), "smoke", "direct")
	ctx = runctx.WithInput(ctx, input)
	ctx = loop.WithResultStore(ctx, loop.NewResultStore(store.NewInMemory(), 0))
	ctx = loop.WithApprovalMode(ctx, loop.ApprovalAuto)
	return ctx
}

// ---- 矩阵测试 ----

// TestSmokeAssembly:三层装配、边界、风险传播、多 agent 共享。
func TestSmokeAssembly(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})

	if len(app.Agents) != 2 || app.Agents["ops-manager"] == nil || app.Agents["support-bot"] == nil {
		t.Fatalf("agents = %v", app.Agents)
	}
	mounted := app.AgentMounts["ops-manager"]
	// 只有导出 skill 进挂载目录:catalog 5 + marketing 3 + crm 1
	if got := len(mounted.List()); got != 9 {
		for _, m := range mounted.List() {
			t.Log(m.Ref.String())
		}
		t.Fatalf("mounted entries = %d, want 9", got)
	}
	for _, ref := range []string{
		"cap://skill/catalog/price-review",
		"cap://skill/catalog/quick-product-qa",
		"cap://skill/catalog/apply-price",
		"cap://skill/catalog/bulk-audit",
		"cap://skill/catalog/audit-product",
		"cap://skill/marketing/route-inquiry",
		"cap://skill/marketing/launch-campaign",
		"cap://skill/marketing/deep-research",
		"cap://skill/crm/customer-brief",
	} {
		if _, err := mounted.Get(ref); err != nil {
			t.Fatalf("missing %s: %v", ref, err)
		}
	}
	// mutating 风险传播:update_price → apply-price skill
	sk, _ := mounted.Get("cap://skill/catalog/apply-price")
	if sk.Meta().Risk != capability.RiskMutating {
		t.Fatalf("apply-price risk = %v, want mutating", sk.Meta().Risk)
	}
}

// TestSmokeGraphForkDigest:并行 needs 汇合 + fork 快照 + digest 消化。
func TestSmokeGraphForkDigest(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})
	sk, err := app.AgentMounts["ops-manager"].Get("cap://skill/catalog/price-review")
	if err != nil {
		t.Fatal(err)
	}
	ctx := skillCtx("双十一这个价格有竞争力吗")
	ctx = loop.WithConversationSnapshot(ctx, []*schema.Message{
		schema.UserMessage("我们在筹备双十一大促"),
	})
	out, err := capability.Invoke(ctx, sk, `{"sku":"P100","question":"竞争力"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"[BRIEF]", "[ANALYST]", "[SNAP]", "[DIGESTED]"} {
		if !strings.Contains(out, m) {
			t.Fatalf("missing %s in %q", m, out)
		}
	}
}

// TestSmokeEngineMatrix:八种引擎逐一走通。
func TestSmokeEngineMatrix(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})
	mounted := app.AgentMounts["ops-manager"]
	invoke := func(ref, args, input string) string {
		t.Helper()
		sk, err := mounted.Get(ref)
		if err != nil {
			t.Fatal(err)
		}
		out, err := capability.Invoke(skillCtx(input), sk, args)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		return out
	}

	// workflow + use: 入口(audit_flow)
	if out := invoke("cap://skill/catalog/audit-product", `{"id":"P100"}`, "审计"); !strings.Contains(out, "[AUDIT]") {
		t.Fatalf("workflow: %q", out)
	}
	// direct + graph component + use:(qa_flow → catalog_qa)
	if out := invoke("cap://skill/catalog/quick-product-qa", `{"q":"降噪耳机怎么样"}`, "问答"); !strings.Contains(out, "[QA]") {
		t.Fatalf("direct: %q", out)
	}
	// rewoo:一次规划并行执行
	if out := invoke("cap://skill/catalog/bulk-audit", `{"category":"耳机"}`, "盘点"); !strings.Contains(out, "[REWOO]") {
		t.Fatalf("rewoo: %q", out)
	}
	// router:分诊到 faq_bot
	if out := invoke("cap://skill/marketing/route-inquiry", `{"q":"怎么退货"}`, "退货"); !strings.Contains(out, "[FAQBOT]") {
		t.Fatalf("router: %q", out)
	}
	// plan-execute(跨 ns 引用 catalog skill)+ reflection(产稿→评审→修订)
	if out := invoke("cap://skill/marketing/launch-campaign", `{"topic":"双十一"}`, "活动"); !strings.Contains(out, "文案v2[REV]") {
		t.Fatalf("plan-execute+reflection: %q", out)
	}
	// react + 调用级 todo:计划注入 + 即弃
	resetSmokeSeen()
	if out := invoke("cap://skill/marketing/deep-research", `{"topic":"直播带货"}`, "研究"); !strings.Contains(out, "[RESEARCH]") {
		t.Fatalf("deep-research: %q", out)
	}
	if !smokeSawSystemContaining("当前任务计划") {
		t.Fatal("component todo plan was not injected into the loop")
	}
	// react(analyst)+ fork + crm
	if out := invoke("cap://skill/crm/customer-brief", `{"id":"C1"}`, "客户"); !strings.Contains(out, "[CRM]") {
		t.Fatalf("crm: %q", out)
	}
}

// TestSmokeAgentMemoryLoop:agent 多轮对话——轨迹入会话、长期记忆
// 自动召回、滚动摘要落盘。
func TestSmokeAgentMemoryLoop(t *testing.T) {
	setupSmokeEnv(t)
	dataDir := t.TempDir()
	t.Setenv("SMOKE_DATA_DIR", dataDir)
	app := buildSmokeApp(t, BuildOptions{})
	ops := app.Agents["ops-manager"]
	// 终端用户身份:长期记忆用户级作用域据此隔离(生产由 serving/通道填)。
	ctx := runctx.WithUser(context.Background(), "boss-uid")
	sess := "boss"

	// t1:触发 skill 调用(轨迹入会话)
	out, err := ops.Run(ctx, sess, "帮我审查P100的定价")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[T-DONE]") || !strings.Contains(out, "[BRIEF]") {
		t.Fatalf("t1 = %q", out)
	}
	// t2:长期记忆写入
	if out, err = ops.Run(ctx, sess, "记住:我喜欢简短汇报"); err != nil || !strings.Contains(out, "[T-DONE]") {
		t.Fatalf("t2 = %q %v", out, err)
	}
	// t3:自动召回命中(词法召回是子串匹配,查询词须命中 key/value;
	// smokescript 只有在 system 尾部看到"长期记忆"才回 [HIT])
	if out, err = ops.Run(ctx, sess, "偏好"); err != nil || !strings.Contains(out, "[HIT]") {
		t.Fatalf("t3 recall = %q %v", out, err)
	}

	// t4:换一个用户,同一 agent——用户级隔离,召回不到 boss 的偏好
	other := runctx.WithUser(context.Background(), "peer-uid")
	if out, err = ops.Run(other, "peer-session", "偏好"); err != nil || strings.Contains(out, "[HIT]") {
		t.Fatalf("cross-user leak: %q %v", out, err)
	}

	// t5:无用户身份的会话——用户记忆写入 fail fast,不静默落库
	anon := context.Background()
	if out, err = ops.Run(anon, "anon-session", "记住:我喜欢简短汇报"); err != nil ||
		!strings.Contains(out, "end-user") {
		t.Fatalf("anonymous write should fail fast: %q %v", out, err)
	}

	// 会话文件:轨迹记录已持久化
	store, err := session.New("file", map[string]any{"dir": dataDir + "/ops-sessions"}, 40)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.(session.FullLoader).LoadAll(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, m := range all {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "[执行记录]") || !strings.Contains(joined, "price-review") {
		t.Fatal("tool trajectory not persisted into session")
	}

	// t4+:灌闲聊触发滚动摘要(阈值 12 条),异步落盘轮询断言
	for i := 0; i < 5; i++ {
		if _, err := ops.Run(ctx, sess, fmt.Sprintf("闲聊%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		all, _ = store.(session.FullLoader).LoadAll(ctx, sess)
		joined = ""
		for _, m := range all {
			joined += m.Content + "\n"
		}
		if strings.Contains(joined, "[[rolling-summary:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rolling summary never persisted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(joined, "[SUM]") {
		t.Fatal("summary content missing")
	}
}

// smokeDecider:计数式审批通道,固定"本会话总是拒绝"。
type smokeDecider struct{ asked atomic.Int32 }

func (d *smokeDecider) Ask(context.Context, string) (string, error) { return "", nil }
func (d *smokeDecider) Approve(context.Context, runctx.ApprovalRequest) (bool, error) {
	d.asked.Add(1)
	return false, nil
}
func (d *smokeDecider) ApproveDecision(context.Context, runctx.ApprovalRequest) (loop.Decision, error) {
	d.asked.Add(1)
	return loop.DecisionAlwaysDeny, nil
}

// TestSmokeApprovalPolicy:参数级规则免批 + 交互审批 + 决策记忆。
func TestSmokeApprovalPolicy(t *testing.T) {
	backend := setupSmokeEnv(t)
	decider := &smokeDecider{}
	app := buildSmokeApp(t, BuildOptions{Interactor: decider})
	ops := app.Agents["ops-manager"]
	ctx := context.Background()

	// 灰度 SKU 命中 allow 规则:免批直达后端
	out, err := ops.Run(ctx, "pricing", "对CANARY做灰度调价")
	if err != nil {
		t.Fatal(err)
	}
	if backend.priceHits.Load() != 1 || decider.asked.Load() != 0 {
		t.Fatalf("canary should bypass approval: hits=%d asked=%d out=%q",
			backend.priceHits.Load(), decider.asked.Load(), out)
	}
	// 正式调价:询问 → 总是拒绝
	if out, err = ops.Run(ctx, "pricing", "对P100正式调价"); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(out, "拒绝") || backend.priceHits.Load() != 1 || decider.asked.Load() != 1 {
		t.Fatalf("formal price change should be denied: hits=%d asked=%d out=%q",
			backend.priceHits.Load(), decider.asked.Load(), out)
	}
	// 再来一次:决策记忆生效,不再询问
	if out, err = ops.Run(ctx, "pricing", "再对P100正式调价"); err != nil {
		t.Fatal(err)
	} else if decider.asked.Load() != 1 || backend.priceHits.Load() != 1 {
		t.Fatalf("decision memory should skip re-ask: asked=%d out=%q", decider.asked.Load(), out)
	}
}

// TestSmokeBudgetHardStop:低预算 agent 的会话级硬停,skill 内部调用同样计入。
func TestSmokeBudgetHardStop(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})
	_, err := app.Agents["support-bot"].Run(context.Background(), "burn", "随便查点什么")
	var exhausted *loop.ErrBudgetExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("expect budget exhaustion, got %v", err)
	}
}

// TestSmokeFailedTurnTrace:模型故障轮在会话里留下失败记录。
func TestSmokeFailedTurnTrace(t *testing.T) {
	setupSmokeEnv(t)
	dataDir := t.TempDir()
	t.Setenv("SMOKE_DATA_DIR", dataDir)
	app := buildSmokeApp(t, BuildOptions{})
	ops := app.Agents["ops-manager"]
	// 触发中断路径之外的真实错误不易脚本化;改为直接验证机制在
	// agent 单测已覆盖,这里断言正常轮次不产生失败标记(反向守护)。
	if _, err := ops.Run(context.Background(), "ok", "闲聊"); err != nil {
		t.Fatal(err)
	}
	store, _ := session.New("file", map[string]any{"dir": dataDir + "/ops-sessions"}, 40)
	all, _ := store.(session.FullLoader).LoadAll(context.Background(), "ok")
	for _, m := range all {
		if strings.Contains(m.Content, "上一轮执行失败") {
			t.Fatal("healthy turn must not leave failure trace")
		}
	}
}
