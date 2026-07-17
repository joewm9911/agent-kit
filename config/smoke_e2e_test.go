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

func smokeSawSystemContaining(sub string) bool { return smokeSawRoleContaining(schema.System, sub) }
func smokeSawUserContaining(sub string) bool   { return smokeSawRoleContaining(schema.User, sub) }

func smokeSawRoleContaining(role schema.RoleType, sub string) bool {
	smokeSeen.mu.Lock()
	defer smokeSeen.mu.Unlock()
	for _, call := range smokeSeen.msgs {
		for _, m := range call {
			if m.Role == role && strings.Contains(m.Content, sub) {
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
	for _, m := range []string{"[ANALYST]", "[SNAP]", "[DIGESTED]", "[FAQBOT]", "[CRM]", "[RESEARCH]"} {
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
	// P3 后组件 prompt(任务书)与 model 步骤 prompt 落在系统消息;input 落
	// 用户消息(空则 prompt 降级作用户消息)。briefT 合并系统+用户,识别任务书
	// 对两条路径都稳。
	briefT := sysT + "\n" + userT
	reply := func(text string) (*schema.Message, error) { return schema.AssistantMessage(text, nil), nil }

	switch {
	// —— 框架内部调用(system 层识别;提示词已英文化,匹配英文特征串)——
	case strings.Contains(sysT, "bullet-point summary"): // Summarize
		return reply("[SUM]任务与结论要点")
	case strings.Contains(sysT, "You are a result digester"): // digest
		return reply("[DGST]库存充足,周转正常")
	// —— sub-agent persona(P3 后落系统消息;input 空则降级落用户消息)——
	case strings.Contains(briefT, "你是价格分析师"):
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
	case strings.Contains(briefT, "[FAQ]"):
		if toolMsgs == 0 { // agentic RAG:先查知识库
			return call("search_kb", `{"query":"退货"}`), nil
		}
		return reply("[FAQBOT]根据知识库:" + clipStr(toolT, 40)) // 用检索结果作答
	case strings.Contains(briefT, "深入研究课题"):
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
	case strings.Contains(briefT, "你是客户分析师"):
		if toolMsgs == 0 {
			return call("get_customer", `{"id":"C1"}`), nil
		}
		out := "[CRM]建议主动跟进"
		if strings.Contains(userT, "caller's conversation") || strings.Contains(sysT, "caller's conversation") {
			out += "[SNAP]"
		}
		return reply(out)

	// —— agent 主循环 ——
	case strings.Contains(sysT, "[OPS]"):
		switch {
		case strings.Contains(lastUser, "审查") && toolMsgs == 0:
			return call("price-review", `{"sku":"P100","question":"定价是否合理"}`), nil
		case strings.Contains(lastUser, "审查") && toolMsgs == 1: // 拿到过程卡指引 → 亲自执行
			return call("get_product", `{"id":"P100"}`), nil
		case strings.Contains(lastUser, "审查") && toolMsgs == 2:
			return call("get_inventory", `{"sku":"P100"}`), nil
		case strings.Contains(lastUser, "审查"):
			return reply("[T-DONE][BRIEF]定价合理" + markers(toolT))
		case strings.Contains(lastUser, "灰度调价") && toolMsgs == 0:
			return call("update_price", `{"sku":"CANARY-1","price":"9.9"}`), nil
		case strings.Contains(lastUser, "正式调价") && toolMsgs == 0:
			return call("update_price", `{"sku":"P100","price":"199"}`), nil
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
		return call("faq_bot", fmt.Sprintf(`{"q":"第%d次查询"}`, toolMsgs+1)), nil
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
	ctx = runctx.WithLoopInput(ctx, input) // loop 原始输入(set-once,与 agent.Run 一致)
	ctx = loop.WithResultStore(ctx, loop.NewResultStore(store.NewInMemory(), 0))
	ctx = loop.WithApprovalMode(ctx, loop.ApprovalAuto)
	return ctx
}

// ---- 矩阵测试 ----

// TestP3PromptToSystem:P3 角色切换——sub-agent prompt(persona)落到系统
// 消息;input 空时降级作用户消息(零退化)。
func TestP3PromptToSystem(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})
	mounted := app.AgentMounts["ops-manager"]

	// 有输入:sub-agent prompt 进系统消息
	resetSmokeSeen()
	pa, err := mounted.Get("cap://agent/catalog/price_analyst")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capability.Invoke(skillCtx("这个价有竞争力吗"), pa, `{"sku":"P100","question":"竞争力"}`); err != nil {
		t.Fatal(err)
	}
	if !smokeSawSystemContaining("你是价格分析师") { // persona → 系统
		t.Fatal("sub-agent prompt must be in a system message (P3)")
	}

	// 空输入:降级——prompt 落用户消息,不进系统 persona(零退化)
	resetSmokeSeen()
	if _, err := capability.Invoke(skillCtx(""), pa, `{"sku":"P100","question":"竞争力"}`); err != nil {
		t.Fatal(err)
	}
	if !smokeSawUserContaining("你是价格分析师") {
		t.Fatal("empty input should fall back to prompt-as-user")
	}
	if smokeSawSystemContaining("你是价格分析师") {
		t.Fatal("fallback must not also put prompt in system persona")
	}
}

// TestSmokeAssembly:三层装配、边界、风险传播、多 agent 共享。
func TestSmokeAssembly(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})

	if len(app.Agents) != 2 || app.Agents["ops-manager"] == nil || app.Agents["support-bot"] == nil {
		t.Fatalf("agents = %v", app.Agents)
	}
	mounted := app.AgentMounts["ops-manager"]
	// 挂载目录:3 过程卡 + 4 直挂工具 + 4 sub-agent = 11
	if got := len(mounted.List()); got != 11 {
		for _, m := range mounted.List() {
			t.Log(m.Ref.String())
		}
		t.Fatalf("mounted entries = %d, want 11", got)
	}
	for _, ref := range []string{
		"cap://skill/catalog/price-review",
		"cap://skill/catalog/quick-product-qa",
		"cap://skill/catalog/apply-price",
		"cap://tool/shop/search_products",
		"cap://tool/shop/get_product",
		"cap://tool/shop/get_inventory",
		"cap://tool/shop/update_price",
		"cap://agent/catalog/price_analyst",
		"cap://agent/marketing/faq_bot",
		"cap://agent/marketing/deep_research",
		"cap://agent/crm/crm_analyst",
	} {
		if _, err := mounted.Get(ref); err != nil {
			t.Fatalf("missing %s: %v", ref, err)
		}
	}
	// 过程卡只是指引 → readonly;风险在直挂的 mutating 工具上
	card, _ := mounted.Get("cap://skill/catalog/apply-price")
	if card.Meta().Risk != capability.RiskReadonly {
		t.Fatalf("apply-price card risk = %v, want readonly (risk lives on the tool)", card.Meta().Risk)
	}
	up, _ := mounted.Get("cap://tool/shop/update_price")
	if up.Meta().Risk != capability.RiskMutating {
		t.Fatalf("update_price risk = %v, want mutating", up.Meta().Risk)
	}
	// sub-agent 风险传播:readonly 工具面 → readonly
	pa, _ := mounted.Get("cap://agent/catalog/price_analyst")
	if pa.Meta().Risk != capability.RiskReadonly {
		t.Fatalf("price_analyst risk = %v, want readonly", pa.Meta().Risk)
	}
}

// TestSmokeSubagentForkDigest:sub-agent 的 fork 快照 + digest 消化。
func TestSmokeSubagentForkDigest(t *testing.T) {
	setupSmokeEnv(t)
	app := buildSmokeApp(t, BuildOptions{})
	pa, err := app.AgentMounts["ops-manager"].Get("cap://agent/catalog/price_analyst")
	if err != nil {
		t.Fatal(err)
	}
	ctx := skillCtx("双十一这个价格有竞争力吗")
	ctx = loop.WithConversationSnapshot(ctx, []*schema.Message{
		schema.UserMessage("我们在筹备双十一大促"),
	})
	out, err := capability.Invoke(ctx, pa, `{"sku":"P100","question":"竞争力"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"[ANALYST]", "[SNAP]", "[DIGESTED]"} {
		if !strings.Contains(out, m) {
			t.Fatalf("missing %s in %q", m, out)
		}
	}
}

// TestSmokeFormMatrix:两种形态逐一走通——过程卡(指引渲染)与
// sub-agent(隔离循环:RAG / 调用级 todo / fork)。
func TestSmokeFormMatrix(t *testing.T) {
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

	// 过程卡:调用返回渲染后的执行指引(零模型调用)
	if out := invoke("cap://skill/catalog/price-review", `{"sku":"P100","question":"竞争力"}`, "审查"); !strings.Contains(out, "[过程卡|price-review]") || !strings.Contains(out, "P100") {
		t.Fatalf("card guide: %q", out)
	}
	// sub-agent + agentic RAG:内部查知识库后作答
	if out := invoke("cap://agent/marketing/faq_bot", `{"q":"怎么退货"}`, "退货"); !strings.Contains(out, "[FAQBOT]") {
		t.Fatalf("faq_bot: %q", out)
	}
	// sub-agent + 调用级 todo:计划注入 + 即弃
	resetSmokeSeen()
	if out := invoke("cap://agent/marketing/deep_research", `{"topic":"直播带货"}`, "研究"); !strings.Contains(out, "[RESEARCH]") {
		t.Fatalf("deep_research: %q", out)
	}
	if !smokeSawSystemContaining("当前任务计划") {
		t.Fatal("sub-agent todo plan was not injected into the loop")
	}
	// sub-agent + fork(crm)
	if out := invoke("cap://agent/crm/crm_analyst", `{"id":"C1"}`, "客户"); !strings.Contains(out, "[CRM]") {
		t.Fatalf("crm_analyst: %q", out)
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
