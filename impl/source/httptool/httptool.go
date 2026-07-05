// Package httptool 把 HTTP 接口声明为 Source:一段配置声明一批接口,
// 每个接口成为一个 cap://tool/<source名>/<接口名> 能力,无需写代码。
//
// 模型产出的参数按 In 字段分发:path 参数替换 URL 占位符,query 参数
// 拼到查询串,body 参数合并为 JSON 请求体。
package httptool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/impl/utils/decode"
	"github.com/joewm9911/agent-kit/source"
)

func init() {
	source.Register("http", func(ctx context.Context, name string, conf map[string]any) (source.Source, error) {
		var cfg SourceConfig
		if err := decode.Config(conf, &cfg); err != nil {
			return nil, err
		}
		caps := make([]capability.Capability, 0, len(cfg.Tools))
		for _, tc := range cfg.Tools {
			if tc.TimeoutSec <= 0 {
				tc.TimeoutSec = cfg.TimeoutSec
			}
			for k, v := range cfg.Headers { // 源级公共头(如认证),接口级可覆盖
				if _, ok := tc.Headers[k]; !ok {
					if tc.Headers == nil {
						tc.Headers = map[string]string{}
					}
					tc.Headers[k] = v
				}
			}
			c, err := New(name, tc)
			if err != nil {
				return nil, err
			}
			caps = append(caps, c)
		}
		return source.Static(name, caps...), nil
	})
}

// SourceConfig 声明一批 HTTP 接口。
type SourceConfig struct {
	Headers    map[string]string `json:"headers"` // 公共请求头
	TimeoutSec int               `json:"timeout_sec"`
	Tools      []Config          `json:"tools"`
}

// Param 描述一个接口参数及其注入位置。
type Param struct {
	Type     string `json:"type"`     // string | number | integer | boolean
	Desc     string `json:"desc"`     // 给模型看的参数说明
	Required bool   `json:"required"` //
	In       string `json:"in"`       // path | query | body,默认:GET→query,其他→body
}

// Config 声明一个 HTTP 接口。
type Config struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Method      string            `json:"method"` // 默认 GET
	URL         string            `json:"url"`    // 支持 {param} 路径占位
	Headers     map[string]string `json:"headers"`
	Params      map[string]Param  `json:"params"`
	Risk        string            `json:"risk"` // 默认按 method 推断:GET→readonly,其他→mutating
	TimeoutSec  int               `json:"timeout_sec"`
	MaxRespLen  int               `json:"max_resp_len"`
}

// New 从配置构造 HTTP 工具能力,namespace 为所属 source 名。
func New(namespace string, cfg Config) (capability.Capability, error) {
	if cfg.Name == "" || cfg.URL == "" {
		return nil, fmt.Errorf("httptool: name and url are required")
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	cfg.Method = strings.ToUpper(cfg.Method)
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 30
	}

	risk, err := capability.ParseRisk(cfg.Risk)
	if err != nil {
		return nil, err
	}
	if cfg.Risk == "" && cfg.Method != http.MethodGet {
		risk = capability.RiskMutating
	}

	params := make(map[string]*schema.ParameterInfo, len(cfg.Params))
	for name, p := range cfg.Params {
		params[name] = &schema.ParameterInfo{
			Type:     dataType(p.Type),
			Desc:     p.Desc,
			Required: p.Required,
		}
	}
	meta := capability.Meta{
		Ref:         capability.Ref{Kind: "tool", Domain: namespace, Name: cfg.Name},
		Description: cfg.Description,
		Params:      schema.NewParamsOneOfByParams(params),
		Risk:        risk,
	}
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}

	return capability.New(meta, func(ctx context.Context, argsJSON string) (string, error) {
		return invoke(ctx, client, cfg, argsJSON)
	}), nil
}

func invoke(ctx context.Context, client *http.Client, cfg Config, argsJSON string) (string, error) {
	var args map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
	}

	rawURL := cfg.URL
	query := url.Values{}
	body := map[string]any{}
	for name, val := range args {
		p := cfg.Params[name]
		in := p.In
		if in == "" {
			if cfg.Method == http.MethodGet {
				in = "query"
			} else {
				in = "body"
			}
		}
		switch in {
		case "path":
			rawURL = strings.ReplaceAll(rawURL, "{"+name+"}", url.PathEscape(fmt.Sprint(val)))
		case "query":
			query.Set(name, fmt.Sprint(val))
		default:
			body[name] = val
		}
	}
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		rawURL += sep + query.Encode()
	}

	var reader io.Reader
	if len(body) > 0 {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, rawURL, reader)
	if err != nil {
		return "", err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	out := string(data)
	if cfg.MaxRespLen > 0 && len(out) > cfg.MaxRespLen {
		out = out[:cfg.MaxRespLen] + "...(truncated)"
	}
	if resp.StatusCode >= 400 {
		// 返回错误详情而非 error:让模型看到响应体,自行决定重试或换参数。
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, out), nil
	}
	return out, nil
}

func dataType(t string) schema.DataType {
	switch t {
	case "number":
		return schema.Number
	case "integer":
		return schema.Integer
	case "boolean":
		return schema.Boolean
	case "array":
		return schema.Array
	case "object":
		return schema.Object
	default:
		return schema.String
	}
}
