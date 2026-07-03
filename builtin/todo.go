// Package builtin 提供框架内置能力:todo(计划外化)与 ask_user
// (人机交互)。它们是"结构进能力"的轻量档——计划只是外化的状态,
// 拆解、推进、完成判断全在大脑。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/runctx"
)

type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// todoStore 按 agent+session 隔离的进程内计划存储。
type todoStore struct {
	mu    sync.Mutex
	lists map[string][]todoItem
}

var todos = &todoStore{lists: map[string][]todoItem{}}

func sessionKey(ctx context.Context) string {
	return runctx.Agent(ctx) + "/" + runctx.Session(ctx)
}

// TodoCapabilities 返回 todo_write / todo_read 两个能力。
func TodoCapabilities() []capability.Capability {
	writeMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "builtin", Name: "todo_write"},
		Description: "写入/更新任务计划清单(整体替换)。多步骤任务开始时列出计划,每完成一项更新状态。",
		Params: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"todos": {
				Type: schema.Array, Required: true, Desc: "完整的任务清单",
				ElemInfo: &schema.ParameterInfo{
					Type: schema.Object,
					SubParams: map[string]*schema.ParameterInfo{
						"content": {Type: schema.String, Desc: "任务内容", Required: true},
						"status":  {Type: schema.String, Desc: "状态", Enum: []string{"pending", "in_progress", "completed"}, Required: true},
					},
				},
			},
		}),
	}
	write := capability.New(writeMeta, func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Todos []todoItem `json:"todos"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("parse todos: %w", err)
		}
		todos.mu.Lock()
		todos.lists[sessionKey(ctx)] = args.Todos
		todos.mu.Unlock()
		return render(args.Todos), nil
	})

	readMeta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Provider: "builtin", Namespace: "builtin", Name: "todo_read"},
		Description: "读取当前任务计划清单。",
		Params:      schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}
	read := capability.New(readMeta, func(ctx context.Context, _ string) (string, error) {
		todos.mu.Lock()
		list := todos.lists[sessionKey(ctx)]
		todos.mu.Unlock()
		if len(list) == 0 {
			return "计划为空。", nil
		}
		return render(list), nil
	})

	return []capability.Capability{write, read}
}

// Snapshot 返回某会话的计划渲染文本,供通道(飞书卡片等)展示进度。
func Snapshot(agentName, sessionID string) string {
	todos.mu.Lock()
	defer todos.mu.Unlock()
	list := todos.lists[agentName+"/"+sessionID]
	if len(list) == 0 {
		return ""
	}
	return render(list)
}

func render(list []todoItem) string {
	var sb strings.Builder
	for _, t := range list {
		mark := "☐"
		switch t.Status {
		case "in_progress":
			mark = "◐"
		case "completed":
			mark = "☑"
		}
		fmt.Fprintf(&sb, "%s %s\n", mark, t.Content)
	}
	return sb.String()
}
