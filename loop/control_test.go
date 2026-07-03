package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joewm9911/agent-kit/capability"
)

func TestControlSteerInjectsIntoResult(t *testing.T) {
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
	}, func(ctx context.Context, in string) (string, error) { return "result", nil })
	wrapped := ControlTools([]capability.Capability{echo})

	cs := &ControlState{}
	ctx := WithControl(context.Background(), cs)

	cs.Steer("改成只查北京的")
	out, err := capability.Invoke(ctx, wrapped[0], "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "用户插话") || !strings.Contains(out, "只查北京") {
		t.Fatalf("got %q", out)
	}
	// 插话只送达一次
	out, _ = capability.Invoke(ctx, wrapped[0], "{}")
	if strings.Contains(out, "插话") {
		t.Fatalf("steer should be delivered once, got %q", out)
	}
}

func TestControlInterruptStopsNextCall(t *testing.T) {
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
	}, func(ctx context.Context, in string) (string, error) { return "result", nil })
	wrapped := ControlTools([]capability.Capability{echo})

	cs := &ControlState{}
	ctx := WithControl(context.Background(), cs)
	cs.Interrupt()

	_, err := capability.Invoke(ctx, wrapped[0], "{}")
	var interrupted *ErrInterrupted
	if !errors.As(err, &interrupted) {
		t.Fatalf("expect ErrInterrupted, got %v", err)
	}

	// 新一轮复位后恢复正常
	cs.BeginTurn(nil)
	if out, err := capability.Invoke(ctx, wrapped[0], "{}"); err != nil || out != "result" {
		t.Fatalf("after reset: %q %v", out, err)
	}
}

func TestControlNoStateZeroOverhead(t *testing.T) {
	echo := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "echo"},
	}, func(ctx context.Context, in string) (string, error) { return "plain", nil })
	wrapped := ControlTools([]capability.Capability{echo})
	if out, err := capability.Invoke(context.Background(), wrapped[0], "{}"); err != nil || out != "plain" {
		t.Fatalf("got %q %v", out, err)
	}
}
