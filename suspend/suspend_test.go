package suspend

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/engine"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/runctx"
)

func TestFileStoreRoundtrip(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("ask", "轮次-键/带斜杠", []byte("值")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := store.Get("ask", "轮次-键/带斜杠")
	if err != nil || !ok || string(v) != "值" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	all, err := store.List("ask")
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %v %v", all, err)
	}
	if err := store.Delete("ask", "轮次-键/带斜杠"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get("ask", "轮次-键/带斜杠"); ok {
		t.Fatal("should be deleted")
	}
}

// TestSuspendResumeAcrossRestart 是核心验收:挂起 → "进程重启"
// (换一个 Journal 实例,仅共享落盘状态)→ 答案到达 → 重放完成,
// 且重放不重复执行已完成的 mutating 操作、不重复提问。
func TestSuspendResumeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	var writeCount, asked int32

	write := capability.New(capability.Meta{
		Ref:  capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "write"},
		Risk: capability.RiskMutating,
	}, func(ctx context.Context, in string) (string, error) {
		atomic.AddInt32(&writeCount, 1)
		return "written", nil
	})
	askUser := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "b", Name: "ask_user"},
	}, func(ctx context.Context, in string) (string, error) {
		return runctx.GetInteractor(ctx).Ask(ctx, "确认发布到生产环境吗?")
	})

	// 每次"进程启动"重建整套运行时(同一轮次 ID,同一落盘目录)
	runTurn := func(turnID string) (string, error) {
		store, err := NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		j := NewJournal(store, turnID)
		m := testmodel.New( // 模型脚本每次进程启动后从头重放
			testmodel.ToolCallMsg("write", `{"file":"a"}`),
			testmodel.ToolCallMsg("ask_user", `{}`),
			schema.AssistantMessage("已发布", nil),
		)
		runner, err := engine.Build(context.Background(), "react", &engine.Assembly{
			Model:        m,
			Capabilities: DurableEffects([]capability.Capability{write, askUser}),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithJournal(context.Background(), j)
		ctx = runctx.WithInteractor(ctx, Interactor(j, func(ctx context.Context, q string) error {
			atomic.AddInt32(&asked, 1)
			return nil // 问题"送达用户"
		}))
		out, err := runner.Generate(ctx, []*schema.Message{schema.UserMessage("发布服务")})
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}

	turnID := "turn-1"

	// 首跑:write 执行 → ask 挂起,整轮退栈
	_, err := runTurn(turnID)
	var suspended *ErrSuspended
	if !errors.As(err, &suspended) {
		t.Fatalf("expect ErrSuspended, got %v", err)
	}
	if writeCount != 1 || asked != 1 {
		t.Fatalf("first run: writes=%d asked=%d", writeCount, asked)
	}

	// "进程重启"后用户回复:答案写入交互日志(只依赖落盘状态)
	store2, _ := NewFileStore(dir)
	if err := AnswerPending(store2, suspended.InteractionID, "确认"); err != nil {
		t.Fatal(err)
	}

	// 重放:write 命中效果日志不再执行,ask 命中交互日志直接返回答案
	out, err := runTurn(turnID)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if out != "已发布" {
		t.Fatalf("got %q", out)
	}
	if writeCount != 1 {
		t.Fatalf("mutating tool must not re-execute on replay, writes = %d", writeCount)
	}
	if asked != 1 {
		t.Fatalf("question must not be re-asked on replay, asked = %d", asked)
	}
}

func TestPendingTurnRoundtrip(t *testing.T) {
	store := NewInMemory()
	rec := PendingTurn{TurnID: "t1", Input: "发布", WaitingID: "t1-abc"}
	if err := SavePendingTurn(store, "sess-1", rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := TakePendingTurn(store, "sess-1")
	if err != nil || !ok || got != rec {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	// Take 即删除
	if _, ok, _ := TakePendingTurn(store, "sess-1"); ok {
		t.Fatal("pending turn should be consumed")
	}
}

func TestApproveSuspendAndReplay(t *testing.T) {
	store := NewInMemory()
	j := NewJournal(store, "t1")
	it := Interactor(j, func(context.Context, string) error { return nil })

	req := runctx.ApprovalRequest{CapRef: "cap://tool.t/x/y", Description: "写文件", Arguments: `{"f":"a"}`}
	_, err := it.Approve(context.Background(), req)
	var suspended *ErrSuspended
	if !errors.As(err, &suspended) {
		t.Fatalf("expect suspend, got %v", err)
	}
	if !strings.Contains(suspended.Question, "批准") {
		t.Fatalf("question = %q", suspended.Question)
	}
	if err := AnswerPending(store, suspended.InteractionID, "同意"); err != nil {
		t.Fatal(err)
	}
	ok, err := it.Approve(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("replay approve: %v %v", ok, err)
	}
}

func TestEffectsCleanupOnComplete(t *testing.T) {
	store := NewInMemory()
	j := NewJournal(store, "t1")
	j.SaveEffect("tool.t/x/y", `{"a":1}`, "done")
	if _, ok := j.Effect("tool.t/x/y", `{"a":1}`); !ok {
		t.Fatal("effect should be recorded")
	}
	j.CompleteTurn()
	if _, ok := j.Effect("tool.t/x/y", `{"a":1}`); ok {
		t.Fatal("effects should be cleaned after turn completes")
	}
}
