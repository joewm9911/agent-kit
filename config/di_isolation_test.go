package config

// P1(进程级全局 store 单例)修复的验收:对象化之前,todo/digest 后端是
// 包级全局,同进程装配多个 agent 时 SetStore/SetResultBackend 后者覆盖
// 前者(last-writer-wins),各 agent 的 store 配置静默失效。本文件断言
// 修复后的行为:每个 agent 各持各的后端实例,写入互不串。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/agent"
	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/store"
)

// bigResultTool 返回固定大结果的工具,供 digest 路径测试。
func bigResultTool(name, out string) capability.Capability {
	return capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: name},
	}, func(context.Context, string) (string, error) { return out, nil })
}

// 记录型 KV 后端:每次构造记下 (实例名 → kv),供断言哪个 agent 写进了
// 哪个后端。
var (
	recMu   sync.Mutex
	recKVs  = map[string]store.KV{}
	recOnce sync.Once
)

func registerRecordingKV() {
	recOnce.Do(func() {
		store.RegisterBackend("reckv", func(conf map[string]any) (store.KV, error) {
			kv := store.NewInMemory()
			name, _ := conf["name"].(string)
			recMu.Lock()
			recKVs[name] = kv
			recMu.Unlock()
			return kv, nil
		})
	})
}

func recordedKV(t *testing.T, name string) store.KV {
	t.Helper()
	recMu.Lock()
	defer recMu.Unlock()
	kv := recKVs[name]
	if kv == nil {
		t.Fatalf("backend %q was never constructed", name)
	}
	return kv
}

// TestMultiAgentTodoStoreIsolation:同进程两个 agent 各配各的 todo 后端,
// 模型各写一份计划(相同 session id),断言各落各的后端、内容互不串。
func TestMultiAgentTodoStoreIsolation(t *testing.T) {
	registerRecordingKV()
	ctx := context.Background()

	build := func(name, storeName string) *agent.Agent {
		m := testmodel.New(
			testmodel.ToolCallMsg("todo_write",
				`{"todos":[{"content":"`+name+` 的计划","status":"in_progress"}]}`),
			schema.AssistantMessage("done", nil),
		)
		ac := &AgentConfig{
			Name: name,
			Stores: []StoreInstance{
				{Name: storeName, Kind: "todo", Type: "reckv", Config: map[string]any{"name": storeName}},
			},
			Todo: TodoConfig{Store: "cap://store/todo/" + storeName},
		}
		a, err := buildAgent(ctx, ac, Profile{}, nil, nil, m, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	// 关键场景:先装配 A 再装配 B(全局单例时代 B 的 SetStore 会覆盖 A),
	// 然后各跑一轮。
	a := build("agent-a", "plans-a")
	b := build("agent-b", "plans-b")
	if _, err := a.Run(ctx, "s1", "做计划"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, "s1", "做计划"); err != nil {
		t.Fatal(err)
	}

	kvA, kvB := recordedKV(t, "plans-a"), recordedKV(t, "plans-b")
	keysA, _ := kvA.Scan(ctx, "")
	keysB, _ := kvB.Scan(ctx, "")
	if len(keysA) != 1 || len(keysB) != 1 {
		t.Fatalf("each agent must write its own backend: plans-a=%d plans-b=%d keys", len(keysA), len(keysB))
	}
	va, _, _ := kvA.Get(ctx, keysA[0])
	vb, _, _ := kvB.Get(ctx, keysB[0])
	if !strings.Contains(string(va), "agent-a 的计划") {
		t.Fatalf("agent-a's plan landed wrong: %s", va)
	}
	if !strings.Contains(string(vb), "agent-b 的计划") {
		t.Fatalf("agent-b's plan landed wrong (last-writer-wins regression): %s", vb)
	}
}

// TestMultiAgentResultStoreIsolation:两个 agent 各配各的 digest 结果暂存
// 后端,大结果消化后的全文各落各的后端。
func TestMultiAgentResultStoreIsolation(t *testing.T) {
	registerRecordingKV()
	ctx := context.Background()
	over := 100
	big := strings.Repeat("原始日志。", 200) // 1000 字符,必触发消化

	build := func(name, storeName string) *agent.Agent {
		m := testmodel.New(
			testmodel.ToolCallMsg("dump_"+name, `{}`),
			schema.AssistantMessage("done", nil),
		)
		ac := &AgentConfig{
			Name: name,
			Stores: []StoreInstance{
				{Name: storeName, Kind: "result", Type: "reckv", Config: map[string]any{"name": storeName}},
			},
		}
		ac.Digest = DigestProfile{Over: &over, Store: "cap://store/result/" + storeName}
		caps := []capability.Capability{bigResultTool("dump_"+name, big)}
		a, err := buildAgent(ctx, ac, ac.Profile, caps, nil, m, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	a := build("agent-a", "results-a")
	b := build("agent-b", "results-b")
	if _, err := a.Run(ctx, "s1", "取日志"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, "s1", "取日志"); err != nil {
		t.Fatal(err)
	}

	keysA, _ := recordedKV(t, "results-a").Scan(ctx, "")
	keysB, _ := recordedKV(t, "results-b").Scan(ctx, "")
	if len(keysA) == 0 || len(keysB) == 0 {
		t.Fatalf("each agent's digest must land in its own backend: results-a=%d results-b=%d keys", len(keysA), len(keysB))
	}
}
