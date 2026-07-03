// Package a2a 把远端 agent 服务接入为 Source:远端跑的是 skill 还是
// 完整 agent 对本地不可见,统一以 cap://agent.a2a/<source名>/<agent名>
// 的黑盒能力挂载。serving 包暴露的正是同一协议,agentkit 部署的服务
// 之间天然互通。
//
// 协议(极简,两个端点):
//
//	GET  {base_url}/a2a/agents
//	  → [{"name": "...", "description": "..."}]
//	POST {base_url}/a2a/agents/{name}/tasks
//	  ← {"task": "..."}
//	  → {"result": "..."} 或 {"error": "..."}
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/registry"
	"github.com/joewm9911/agent-kit/source"
)

func init() {
	source.Register("a2a", func(ctx context.Context, name string, conf map[string]any) (source.Source, error) {
		var cfg Config
		if err := registry.DecodeConfig(conf, &cfg); err != nil {
			return nil, err
		}
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("a2a source %q: base_url is required", name)
		}
		if cfg.TimeoutSec <= 0 {
			cfg.TimeoutSec = 300 // 远端 agent 跑长任务,超时给足
		}
		// 远端 agent 的副作用不可见,默认按 mutating 对待,由接入方显式放宽。
		if cfg.Risk == "" {
			cfg.Risk = "mutating"
		}
		return &a2aSource{name: name, cfg: cfg,
			client: &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}}, nil
	})
}

// Config 声明一个远端 agent 服务。
type Config struct {
	BaseURL    string            `json:"base_url"`
	Headers    map[string]string `json:"headers"`
	Agents     []string          `json:"agents"` // 只接入这些 agent;空 = 全部
	Risk       string            `json:"risk"`
	TimeoutSec int               `json:"timeout_sec"`
}

type agentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type a2aSource struct {
	name   string
	cfg    Config
	client *http.Client
}

func (s *a2aSource) Name() string { return s.name }

func (s *a2aSource) Sync(ctx context.Context) ([]capability.Capability, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/a2a/agents", nil)
	if err != nil {
		return nil, err
	}
	s.setHeaders(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a list agents: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("a2a list agents: HTTP %d: %s", resp.StatusCode, body)
	}
	var agents []agentInfo
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return nil, fmt.Errorf("a2a decode agents: %w", err)
	}

	want := map[string]bool{}
	for _, a := range s.cfg.Agents {
		want[a] = true
	}
	risk, err := capability.ParseRisk(s.cfg.Risk)
	if err != nil {
		return nil, err
	}

	var caps []capability.Capability
	for _, a := range agents {
		if len(want) > 0 && !want[a.Name] {
			continue
		}
		caps = append(caps, s.wrap(a, risk))
	}
	return caps, nil
}

func (s *a2aSource) wrap(a agentInfo, risk capability.Risk) capability.Capability {
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "agent", Provider: "a2a", Namespace: s.name, Name: a.Name},
		Description: a.Description,
		Params:      capability.SingleParam("task", "交给该远端 agent 的完整任务描述"),
		Risk:        risk,
	}
	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		task := capability.ParseSingle(argsJSON, "task")
		return s.invoke(ctx, a.Name, task)
	})
}

func (s *a2aSource) invoke(ctx context.Context, agentName, task string) (string, error) {
	body, _ := json.Marshal(map[string]string{"task": task})
	u := fmt.Sprintf("%s/a2a/agents/%s/tasks", s.cfg.BaseURL, url.PathEscape(agentName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	s.setHeaders(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Sprintf("a2a error: %v", err), nil // 回传模型,让它决定下一步
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("a2a decode result: %w", err)
	}
	if out.Error != "" {
		return fmt.Sprintf("a2a remote error: %s", out.Error), nil
	}
	return out.Result, nil
}

func (s *a2aSource) setHeaders(req *http.Request) {
	for k, v := range s.cfg.Headers {
		req.Header.Set(k, v)
	}
}
