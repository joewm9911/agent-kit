// Package mcptool 把 MCP server 接入为 Source:server 的每个工具
// 成为一个 cap://tool.mcp/<source名>/<工具名> 能力。
package mcptool

import (
	"context"
	"fmt"
	"os"

	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/registry"
	"github.com/joewm9911/agent-kit/source"
)

func init() {
	source.Register("mcp", func(ctx context.Context, name string, conf map[string]any) (source.Source, error) {
		var cfg Config
		if err := registry.DecodeConfig(conf, &cfg); err != nil {
			return nil, err
		}
		return &mcpSource{name: name, cfg: cfg}, nil
	})
}

// Config 声明一个 MCP server 连接。
type Config struct {
	Transport string            `json:"transport"` // stdio | sse | http
	Command   string            `json:"command"`   // stdio:启动命令
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`     // sse / http:server 地址
	Headers   map[string]string `json:"headers"` // 传给 server 的自定义头
	Tools     []string          `json:"tools"`   // 只接入这些工具;空 = 全部
	Version   string            `json:"version"` // 记入 Ref.Version,便于回溯
	// Risk 是该 server 工具的默认风险级别(readonly/mutating/dangerous),
	// RiskOverrides 按工具名覆盖。MCP 协议不传风险语义,只能由接入方声明。
	Risk          string            `json:"risk"`
	RiskOverrides map[string]string `json:"risk_overrides"`
}

type mcpSource struct {
	name string
	cfg  Config
	cli  *mcpclient.Client
}

func (s *mcpSource) Name() string { return s.name }

func (s *mcpSource) Sync(ctx context.Context) ([]capability.Capability, error) {
	if s.cli == nil {
		cli, err := connect(ctx, s.cfg)
		if err != nil {
			return nil, err
		}
		s.cli = cli
	}

	tools, err := einomcp.GetTools(ctx, &einomcp.Config{
		Cli:           s.cli,
		ToolNameList:  s.cfg.Tools,
		CustomHeaders: s.cfg.Headers,
	})
	if err != nil {
		s.cli = nil // 连接可能已坏,下次 Sync 重连
		return nil, fmt.Errorf("mcp get tools: %w", err)
	}

	defaultRisk, err := capability.ParseRisk(s.cfg.Risk)
	if err != nil {
		return nil, err
	}

	caps := make([]capability.Capability, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, err
		}
		risk := defaultRisk
		if o, ok := s.cfg.RiskOverrides[info.Name]; ok {
			if risk, err = capability.ParseRisk(o); err != nil {
				return nil, err
			}
		}
		ref := capability.Ref{
			Kind: "tool", Provider: "mcp", Namespace: s.name,
			Name: info.Name, Version: s.cfg.Version,
		}
		c, err := capability.FromTool(ctx, t, ref, risk)
		if err != nil {
			return nil, err
		}
		caps = append(caps, c)
	}
	return caps, nil
}

func connect(ctx context.Context, cfg Config) (*mcpclient.Client, error) {
	cli, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := cli.Start(ctx); err != nil {
		return nil, fmt.Errorf("mcp start: %w", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "agentkit", Version: "0.1.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}
	return cli, nil
}

func newClient(cfg Config) (*mcpclient.Client, error) {
	switch cfg.Transport {
	case "", "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcp stdio: command is required")
		}
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		return mcpclient.NewStdioMCPClient(cfg.Command, env, cfg.Args...)
	case "sse":
		return mcpclient.NewSSEMCPClient(cfg.URL)
	case "http", "streamable":
		return mcpclient.NewStreamableHttpClient(cfg.URL)
	default:
		return nil, fmt.Errorf("mcp: unknown transport %q", cfg.Transport)
	}
}
