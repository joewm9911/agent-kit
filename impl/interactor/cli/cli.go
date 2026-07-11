// Package cli 提供 runctx.Interactor 的终端内置实现。
// CLI 用于终端运行;IM 通道(飞书等)在 channel 包中提供各自实现。
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/runtime/loop"
)

// CLI 是终端交互通道:ask_user 阻塞读 stdin,审批以 y/n 确认。
// stdin 由单一读取协程供给行通道:阻塞读无法被 ctx 打断,放在调用
// 栈里会让取消/超时永远等不到返回且泄漏持锁 goroutine;经通道中转后
// 取消立即返回,迟到的输入行留在通道里给下一次提问(不丢同步)。
type CLI struct {
	mu    sync.Mutex // 同一时刻只允许一个问题占用终端
	out   io.Writer
	lines chan lineResult
	once  sync.Once
	in    *bufio.Reader
}

type lineResult struct {
	text string
	err  error
}

// NewCLI 创建终端交互通道。
func NewCLI() *CLI {
	return &CLI{in: bufio.NewReader(os.Stdin), out: os.Stdout, lines: make(chan lineResult, 1)}
}

// readLine 等待下一行输入或 ctx 取消。读协程惰性启动、进程级常驻。
func (c *CLI) readLine(ctx context.Context) (string, error) {
	c.once.Do(func() {
		go func() {
			for {
				line, err := c.in.ReadString('\n')
				c.lines <- lineResult{text: line, err: err}
				if err != nil {
					return
				}
			}
		}()
	})
	select {
	case r := <-c.lines:
		if r.err != nil {
			return "", r.err
		}
		return strings.TrimSpace(r.text), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *CLI) Ask(ctx context.Context, question string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[agent 提问] %s\n> ", question)
	return c.readLine(ctx)
}

func (c *CLI) Approve(ctx context.Context, req runctx.ApprovalRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[需要批准] %s\n  说明: %s\n  参数: %s\n允许执行? [y/N] ", req.CapRef, req.Description, req.Arguments)
	line, err := c.readLine(ctx)
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(line)
	return ans == "y" || ans == "yes", nil
}

// ApproveDecision 实现 loop.DecisionInteractor:在 y/n 之外提供
// "本会话总是允许/总是拒绝"两档,答案由审批闸门记入决策记忆。
func (c *CLI) ApproveDecision(ctx context.Context, req runctx.ApprovalRequest) (loop.Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[需要批准] %s\n  说明: %s\n  参数: %s\n允许执行? [y=允许 / n=拒绝 / a=本会话总是允许 / x=本会话总是拒绝] ",
		req.CapRef, req.Description, req.Arguments)
	line, err := c.readLine(ctx)
	if err != nil {
		return loop.DecisionDeny, err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return loop.DecisionAllow, nil
	case "a", "always":
		return loop.DecisionAlwaysAllow, nil
	case "x", "never":
		return loop.DecisionAlwaysDeny, nil
	default:
		return loop.DecisionDeny, nil
	}
}
