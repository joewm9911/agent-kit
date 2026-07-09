package todo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// td 是共享的 todo 持有对象(进程内后端);各测试用不同 (agent,session) 键,互不干扰。
var td = New(store.NewInMemory(), 0)

func todoWriteCap(t *testing.T) capability.Capability {
	t.Helper()
	return td.Capabilities()[0]
}

func todoReadCap(t *testing.T) capability.Capability {
	t.Helper()
	return td.Capabilities()[1]
}

func testCtx(agent, session string) context.Context {
	return runctx.With(context.Background(), agent, session)
}

func writeTodos(t *testing.T, ctx context.Context, body string) string {
	t.Helper()
	out, err := capability.Invoke(ctx, todoWriteCap(t), body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTodoValidation(t *testing.T) {
	ctx := testCtx("a", "val")

	// 非法 status:拒绝并纠正,不落库
	out := writeTodos(t, ctx, `{"todos":[{"content":"x","status":"done"}]}`)
	if !strings.Contains(out, "只能是") {
		t.Fatalf("got %q", out)
	}
	if td.Snapshot("a", "val") != "" {
		t.Fatal("rejected write must not persist")
	}

	// 两个 in_progress:拒绝
	out = writeTodos(t, ctx, `{"todos":[
		{"content":"x","status":"in_progress"},
		{"content":"y","status":"in_progress"}]}`)
	if !strings.Contains(out, "一次只做一件事") {
		t.Fatalf("got %q", out)
	}

	// 空 content:拒绝
	out = writeTodos(t, ctx, `{"todos":[{"content":"  ","status":"pending"}]}`)
	if !strings.Contains(out, "为空") {
		t.Fatalf("got %q", out)
	}

	// 重复任务:拒绝
	out = writeTodos(t, ctx, `{"todos":[
		{"content":"x","status":"pending"},
		{"content":"x","status":"pending"}]}`)
	if !strings.Contains(out, "重复") {
		t.Fatalf("got %q", out)
	}

	// 超量:拒绝
	var sb strings.Builder
	sb.WriteString(`{"todos":[`)
	for i := 0; i < 51; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"content":"任务%d","status":"pending"}`, i)
	}
	sb.WriteString(`]}`)
	out = writeTodos(t, ctx, sb.String())
	if !strings.Contains(out, "上限") {
		t.Fatalf("got %q", out)
	}
}

func TestTodoWriteReadAndClear(t *testing.T) {
	ctx := testCtx("a", "wr")
	out := writeTodos(t, ctx, `{"todos":[
		{"content":"查日志","status":"in_progress","active_form":"正在查日志"},
		{"content":"写报告","status":"pending"}]}`)
	if !strings.Contains(out, "◐ 查日志(正在查日志)") || !strings.Contains(out, "☐ 写报告") {
		t.Fatalf("render: %q", out)
	}
	read, _ := capability.Invoke(ctx, todoReadCap(t), `{}`)
	if !strings.Contains(read, "查日志") {
		t.Fatalf("read: %q", read)
	}
	if td.Snapshot("a", "wr") == "" {
		t.Fatal("snapshot should show plan")
	}

	// 空列表 = 清空,返回明确文案
	out = writeTodos(t, ctx, `{"todos":[]}`)
	if out != "计划已清空。" {
		t.Fatalf("got %q", out)
	}
	if td.Snapshot("a", "wr") != "" {
		t.Fatal("cleared plan should be gone")
	}

	// Clear API
	writeTodos(t, ctx, `{"todos":[{"content":"x","status":"pending"}]}`)
	td.Clear("a", "wr")
	if td.Snapshot("a", "wr") != "" {
		t.Fatal("Clear should remove the plan")
	}
}

func TestTodoScopeIsolation(t *testing.T) {
	host := testCtx("a", "iso")
	writeTodos(t, host, `{"todos":[{"content":"宿主计划","status":"in_progress"}]}`)

	// 子执行域:压域后分键,写入不覆盖宿主
	sub := runctx.WithScopePush(host, "sub:helper")
	writeTodos(t, sub, `{"todos":[{"content":"子计划","status":"pending"}]}`)

	hostRead, _ := capability.Invoke(host, todoReadCap(t), `{}`)
	if !strings.Contains(hostRead, "宿主计划") || strings.Contains(hostRead, "子计划") {
		t.Fatalf("host plan polluted: %q", hostRead)
	}
	subRead, _ := capability.Invoke(sub, todoReadCap(t), `{}`)
	if !strings.Contains(subRead, "子计划") || strings.Contains(subRead, "宿主计划") {
		t.Fatalf("sub plan wrong: %q", subRead)
	}
}

func TestTodoKeyCollisionResistant(t *testing.T) {
	// agent "a/b" + session "c" 与 agent "a" + session "b/c" 必须是不同的键
	writeTodos(t, testCtx("a/b", "c"), `{"todos":[{"content":"甲","status":"pending"}]}`)
	writeTodos(t, testCtx("a", "b/c"), `{"todos":[{"content":"乙","status":"pending"}]}`)
	if got := td.Snapshot("a/b", "c"); !strings.Contains(got, "甲") || strings.Contains(got, "乙") {
		t.Fatalf("key collision: %q", got)
	}
}

func TestTodoPlanSection(t *testing.T) {
	ctx := testCtx("a", "plan")
	if td.PlanSection(ctx) != "" {
		t.Fatal("empty plan should not inject")
	}
	writeTodos(t, ctx, `{"todos":[{"content":"做事","status":"in_progress"}]}`)
	sec := td.PlanSection(ctx)
	if !strings.Contains(sec, "当前任务计划") || !strings.Contains(sec, "做事") {
		t.Fatalf("got %q", sec)
	}
}

func TestTodoNudge(t *testing.T) {
	ctx := testCtx("a", "nudge")
	work := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "work"},
	}, func(_ context.Context, _ string) (string, error) { return "done", nil })
	wrapped := td.Nudge([]capability.Capability{work})[0]

	// 没有进行中任务:永不提醒
	for i := 0; i < 6; i++ {
		out, _ := capability.Invoke(ctx, wrapped, `{}`)
		if strings.Contains(out, "计划提醒") {
			t.Fatal("no in_progress item, must not nudge")
		}
	}

	// 有进行中任务:第 5 次非 todo 调用触发提醒,随后计数重置
	writeTodos(t, ctx, `{"todos":[{"content":"排查","status":"in_progress"}]}`)
	var nudgedAt []int
	for i := 1; i <= 10; i++ {
		out, _ := capability.Invoke(ctx, wrapped, `{}`)
		if strings.Contains(out, "计划提醒") {
			nudgedAt = append(nudgedAt, i)
		}
	}
	if len(nudgedAt) != 2 || nudgedAt[0] != 5 || nudgedAt[1] != 10 {
		t.Fatalf("nudged at %v, want [5 10]", nudgedAt)
	}

	// todo_write 更新计划重置计数
	for i := 0; i < 3; i++ {
		capability.Invoke(ctx, wrapped, `{}`)
	}
	writeTodos(t, ctx, `{"todos":[{"content":"排查","status":"completed"},{"content":"修复","status":"in_progress"}]}`)
	for i := 1; i <= 4; i++ {
		out, _ := capability.Invoke(ctx, wrapped, `{}`)
		if strings.Contains(out, "计划提醒") {
			t.Fatalf("counter should reset after todo_write, nudged at %d", i)
		}
	}
}

func TestLoopPromptVariantConsistency(t *testing.T) {
	// 裁剪版 L1 不得提及 todo(提示词与工具面一致性的回归保护)
	if strings.Contains(loop.DefaultLoopPromptNoTodo, "todo") {
		t.Fatal("no-todo L1 variant still mentions todo")
	}
	// 完整版必须仍有 todo 指引(别把主循环的纪律裁没了)
	if !strings.Contains(loop.DefaultLoopPrompt, "todo_write") {
		t.Fatal("default L1 lost its todo discipline")
	}
}

// TestTodoFinishCheck 验证收口守卫的触发矩阵:本轮写过计划且有未收口项
// → 催一次(每轮限一次);全 completed / 本轮没写过 / 无轮语义 → 不催。
func TestTodoFinishCheck(t *testing.T) {
	// 无轮语义(未经 agent 入口):不介入
	bare := testCtx("a", "fc0")
	writeTodos(t, bare, `{"todos":[{"content":"x","status":"pending"}]}`)
	if msg := td.FinishCheck(bare); msg != "" {
		t.Fatalf("no turn state must not nag, got %q", msg)
	}

	// 本轮写过 + 有未收口项:催一次,第二次去重
	ctx := runctx.WithTurnState(testCtx("a", "fc1"))
	writeTodos(t, ctx, `{"todos":[{"content":"x","status":"in_progress","active_form":"做x"},{"content":"y","status":"pending"}]}`)
	msg := td.FinishCheck(ctx)
	if !strings.Contains(msg, "2 项未收口") {
		t.Fatalf("expect nag with open count, got %q", msg)
	}
	if again := td.FinishCheck(ctx); again != "" {
		t.Fatalf("must nag at most once per turn, got %q", again)
	}

	// 全部 completed:不催
	ctx2 := runctx.WithTurnState(testCtx("a", "fc2"))
	writeTodos(t, ctx2, `{"todos":[{"content":"x","status":"completed"}]}`)
	if msg := td.FinishCheck(ctx2); msg != "" {
		t.Fatalf("all completed must not nag, got %q", msg)
	}

	// 残留计划、本轮没写过:不催(交给 PlanSection 的遗留标注)
	writeTodos(t, runctx.WithTurnState(testCtx("a", "fc3")),
		`{"todos":[{"content":"old","status":"pending"}]}`)
	newTurn := runctx.WithTurnState(testCtx("a", "fc3"))
	if msg := td.FinishCheck(newTurn); msg != "" {
		t.Fatalf("untouched stale plan must not nag, got %q", msg)
	}
}

// TestGoalCheck 验证目标达成核对(U4.1):用过计划的轮次收尾时核对一次,
// 提示带原始目标;每轮至多一次;纯问答轮(没写过计划)不介入。
func TestGoalCheck(t *testing.T) {
	// 纯问答轮:本轮没写过计划 → 不介入(不给简单问答加负担)
	noplan := runctx.WithInput(runctx.WithTurnState(testCtx("a", "gc0")), "简单问题")
	if msg := td.GoalCheck(noplan); msg != "" {
		t.Fatalf("no-plan turn must skip goal check, got %q", msg)
	}

	// 用过计划 + 有原始目标:核对一次,提示带目标原文
	ctx := runctx.WithInput(runctx.WithTurnState(testCtx("a", "gc1")), "查 A 再核对全覆盖")
	writeTodos(t, ctx, `{"todos":[{"content":"查A","status":"completed"}]}`)
	msg := td.GoalCheck(ctx)
	if !strings.Contains(msg, "目标达成核对") || !strings.Contains(msg, "查 A 再核对全覆盖") {
		t.Fatalf("goal check must carry the original goal, got %q", msg)
	}
	// 每轮至多一次(硬边界,防死循环)
	if again := td.GoalCheck(ctx); again != "" {
		t.Fatalf("goal check must fire at most once per turn, got %q", again)
	}

	// 无轮语义:不介入
	bare := runctx.WithInput(testCtx("a", "gc2"), "x")
	writeTodos(t, bare, `{"todos":[{"content":"x","status":"completed"}]}`)
	if msg := td.GoalCheck(bare); msg != "" {
		t.Fatalf("no turn state must not check, got %q", msg)
	}
}

// TestTodoPlanSectionStale 验证遗留计划标注:同轮写入后是"当前任务计划",
// 新轮未写入时降为"遗留任务计划"并指示清理;写入后恢复。
func TestTodoPlanSectionStale(t *testing.T) {
	turn1 := runctx.WithTurnState(testCtx("a", "stale"))
	writeTodos(t, turn1, `{"todos":[{"content":"x","status":"pending"}]}`)
	if s := td.PlanSection(turn1); !strings.Contains(s, "当前任务计划") {
		t.Fatalf("same turn must render active header, got %q", s)
	}

	turn2 := runctx.WithTurnState(testCtx("a", "stale"))
	if s := td.PlanSection(turn2); !strings.Contains(s, "遗留任务计划") || !strings.Contains(s, "提交空 todos") {
		t.Fatalf("new turn must render stale header with cleanup hint, got %q", s)
	}
	writeTodos(t, turn2, `{"todos":[{"content":"y","status":"in_progress"}]}`)
	if s := td.PlanSection(turn2); !strings.Contains(s, "当前任务计划") {
		t.Fatalf("after write the header must be active again, got %q", s)
	}

	// 无轮语义:保持既有行为(当前任务计划)
	if s := td.PlanSection(testCtx("a", "stale")); !strings.Contains(s, "当前任务计划") {
		t.Fatalf("nil turn state keeps active header, got %q", s)
	}
}
