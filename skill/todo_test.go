package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/prompt"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/todo"
)

// newTestTodo 模拟装配层注入:进程内后端、无 TTL。
func newTestTodo() *todo.Todo { return todo.New(store.NewInMemory(), 0) }

// TestComponentTodoOptIn 验证调用级临时清单:内部循环可用 todo、
// 宿主计划不受影响、调用结束即弃。
func TestComponentTodoOptIn(t *testing.T) {
	// 宿主(agent 主循环)已有自己的计划
	hostTodo := newTestTodo()
	hostCtx := runctx.With(context.Background(), "host", "s-todo")
	hostWrite := hostTodo.Capabilities()[0]
	if out, err := capability.Invoke(hostCtx, hostWrite,
		`{"todos":[{"content":"宿主计划","status":"in_progress"}]}`); err != nil || !strings.Contains(out, "宿主计划") {
		t.Fatalf("host plan: %q %v", out, err)
	}

	// 组件:todo: true,内部脚本先写清单再收尾
	m := testmodel.New(
		testmodel.ToolCallMsg("todo_write", `{"todos":[{"content":"组件内步骤","status":"in_progress"}]}`),
		schema.AssistantMessage("done", nil),
	)
	sk, err := Build(context.Background(), &Declaration{
		Engine: "react", // 结构决定形态:声明 engine = 子执行体(mode 已移除)
		Name:   "t/researcher",
		Prompt: prompt.Value{Literal: "研究 {input}"},
		Todo:   true,
	}, Deps{DefaultModel: m, Todo: newTestTodo()})
	if err != nil {
		t.Fatal(err)
	}

	out, err := capability.Invoke(hostCtx, sk, `{"input":"x"}`)
	if err != nil || out != "done" {
		t.Fatalf("got %q %v", out, err)
	}

	// 宿主计划原封不动(执行域分键,未被组件覆盖)
	if snap := hostTodo.Snapshot("host", "s-todo"); !strings.Contains(snap, "宿主计划") || strings.Contains(snap, "组件内步骤") {
		t.Fatalf("host plan polluted: %q", snap)
	}

	// 再次调用:新执行域,上次的清单不可见(即弃 + 域唯一双保险)
	m2read := testmodel.New(
		testmodel.ToolCallMsg("todo_read", `{}`),
		schema.AssistantMessage("checked", nil),
	)
	sk2, err := Build(context.Background(), &Declaration{
		Engine: "react", // 结构决定形态:声明 engine = 子执行体(mode 已移除)
		Name:   "t/researcher2",
		Prompt: prompt.Value{Literal: "研究 {input}"},
		Todo:   true,
	}, Deps{DefaultModel: m2read, Todo: newTestTodo()})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := capability.Invoke(hostCtx, sk2, `{"input":"y"}`); err != nil || out != "checked" {
		t.Fatalf("got %q %v", out, err)
	}
}

// TestComponentTodoEngineRestriction 验证 todo 只允许 react。
func TestComponentTodoEngineRestriction(t *testing.T) {
	_, err := Build(context.Background(), &Declaration{
		Name:   "t/bad",
		Prompt: prompt.Value{Literal: "x"},
		Engine: "plan-execute",
		Todo:   true,
	}, Deps{DefaultModel: testmodel.New()})
	if err == nil || !strings.Contains(err.Error(), "only makes sense for react") {
		t.Fatalf("expect engine restriction, got %v", err)
	}
}

// TestComponentL1MatchesToolFace 验证 L1 与工具面一致:
// 开 todo 的组件提示词含纪律指引,不开的不含。
func TestComponentL1MatchesToolFace(t *testing.T) {
	build := func(todo bool) []*schema.Message {
		em := &echoInputModel{}
		name := "t/plain"
		if todo {
			name = "t/todoful"
		}
		sk, err := Build(context.Background(), &Declaration{
			Engine: "react", // 结构决定形态:声明 engine = 子执行体(mode 已移除)
			Name:   name,
			Prompt: prompt.Value{Literal: "做 {input}"},
			Todo:   todo,
		}, Deps{DefaultModel: em, Todo: newTestTodo()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := capability.Invoke(context.Background(), sk, `{"input":"x"}`); err != nil {
			t.Fatal(err)
		}
		return em.seen[0]
	}

	withTodo := build(true)
	if !strings.Contains(withTodo[0].Content, "todo_write") {
		t.Fatal("todo-enabled component should keep the discipline guidance in L1")
	}
	without := build(false)
	if strings.Contains(without[0].Content, "todo") {
		t.Fatal("todo-disabled component must not mention todo in L1")
	}
}
