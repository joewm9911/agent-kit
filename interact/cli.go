// Package interact 提供 runctx.Interactor 的内置实现。
// CLI 用于终端运行;IM 通道(飞书等)在 channel 包中提供各自实现。
package interact

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/joewm9911/agent-kit/loop"
	"github.com/joewm9911/agent-kit/runctx"
)

// CLI 是终端交互通道:ask_user 阻塞读 stdin,审批以 y/n 确认。
type CLI struct {
	mu  sync.Mutex // 同一时刻只允许一个问题占用终端
	in  *bufio.Reader
	out io.Writer
}

// NewCLI 创建终端交互通道。
func NewCLI() *CLI {
	return &CLI{in: bufio.NewReader(os.Stdin), out: os.Stdout}
}

func (c *CLI) Ask(_ context.Context, question string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[agent 提问] %s\n> ", question)
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c *CLI) Approve(_ context.Context, req runctx.ApprovalRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[需要批准] %s\n  说明: %s\n  参数: %s\n允许执行? [y/N] ", req.CapRef, req.Description, req.Arguments)
	line, err := c.in.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// ApproveDecision 实现 loop.DecisionInteractor:在 y/n 之外提供
// "本会话总是允许/总是拒绝"两档,答案由审批闸门记入决策记忆。
func (c *CLI) ApproveDecision(_ context.Context, req runctx.ApprovalRequest) (loop.Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\n[需要批准] %s\n  说明: %s\n  参数: %s\n允许执行? [y=允许 / n=拒绝 / a=本会话总是允许 / x=本会话总是拒绝] ",
		req.CapRef, req.Description, req.Arguments)
	line, err := c.in.ReadString('\n')
	if err != nil {
		return loop.DecisionDeny, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
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
