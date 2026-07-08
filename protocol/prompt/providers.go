package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"
)

func init() {
	Register("inline", newInline)
	Register("file", newFile)
	Register("http", newHTTP)
}

// ---- inline:配置里直接写,零依赖的默认实现 ----

type inlineProvider struct {
	prompts map[string]string
}

func newInline(conf map[string]any) (Provider, error) {
	p := &inlineProvider{prompts: map[string]string{}}
	for k, v := range conf {
		p.prompts[k] = fmt.Sprint(v)
	}
	return p, nil
}

func (p *inlineProvider) Get(_ context.Context, name, _ string) (*Template, error) {
	text, ok := p.prompts[name]
	if !ok {
		return nil, fmt.Errorf("inline prompt %q not defined", name)
	}
	return &Template{Name: name, Version: "inline", Text: text}, nil
}

// ---- file:<name>.md,和代码一起评审。以 fs.FS 子树承载,与配置同源
// (本地目录 / 内嵌二进制 / 远程),锚点是资源 FS 而非进程 CWD ----

type fileProvider struct {
	fsys fs.FS
}

// NewFileProvider 从一个 fs.FS 子树取 <name>.md。装配层(BuildApp)以资源
// FS 在 prompt 目录处 fs.Sub 出这个子树,于是 prompt 与配置同源。
func NewFileProvider(fsys fs.FS) Provider {
	return &fileProvider{fsys: fsys}
}

// newFile 是注册表工厂,供单文件 Build(无资源 FS)使用:dir 以 os.DirFS
// 锚定。多文件 BuildApp 走 NewFileProvider + 资源 FS。
func newFile(conf map[string]any) (Provider, error) {
	dir, _ := conf["dir"].(string)
	if dir == "" {
		return nil, fmt.Errorf("file prompt provider: dir is required")
	}
	return &fileProvider{fsys: os.DirFS(dir)}, nil
}

func (p *fileProvider) Get(_ context.Context, name, _ string) (*Template, error) {
	// fs.FS 语义拒绝 '..' 逃逸,顺带把读到子树外的路径穿越堵死。
	data, err := fs.ReadFile(p.fsys, path.Clean(name)+".md")
	if err != nil {
		return nil, fmt.Errorf("read prompt file: %w", err)
	}
	sum := sha256.Sum256(data)
	return &Template{Name: name, Version: hex.EncodeToString(sum[:8]), Text: string(data)}, nil
}

// ---- http:通用提示词平台适配,带本地缓存 + TTL,平台不可达时降级用缓存 ----
//
// 协议约定:GET {base_url}/prompts/{name}?label={label}
// 响应:{"name": "...", "version": "...", "text": "..."}
// Langfuse 等平台可通过其兼容网关或一层薄代理接入。

type httpProvider struct {
	baseURL string
	headers map[string]string
	client  *http.Client
	ttl     time.Duration

	mu    sync.RWMutex
	cache map[string]cached
}

type cached struct {
	tpl *Template
	at  time.Time
}

func newHTTP(conf map[string]any) (Provider, error) {
	b, _ := json.Marshal(conf)
	var cfg struct {
		BaseURL    string            `json:"base_url"`
		Headers    map[string]string `json:"headers"`
		TimeoutSec int               `json:"timeout_sec"`
		TTLSec     int               `json:"ttl_sec"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("http prompt provider: base_url is required")
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 10
	}
	if cfg.TTLSec <= 0 {
		cfg.TTLSec = 300
	}
	return &httpProvider{
		baseURL: cfg.BaseURL,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second},
		ttl:     time.Duration(cfg.TTLSec) * time.Second,
		cache:   map[string]cached{},
	}, nil
}

func (p *httpProvider) Get(ctx context.Context, name, label string) (*Template, error) {
	key := name + "@" + label
	p.mu.RLock()
	c, ok := p.cache[key]
	p.mu.RUnlock()
	if ok && time.Since(c.at) < p.ttl {
		return c.tpl, nil
	}

	tpl, err := p.fetch(ctx, name, label)
	if err != nil {
		if ok { // 平台不可达:降级用过期缓存
			return c.tpl, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.cache[key] = cached{tpl: tpl, at: time.Now()}
	p.mu.Unlock()
	return tpl, nil
}

func (p *httpProvider) fetch(ctx context.Context, name, label string) (*Template, error) {
	u := fmt.Sprintf("%s/prompts/%s?label=%s", p.baseURL, url.PathEscape(name), url.QueryEscape(label))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("prompt platform returned %d: %s", resp.StatusCode, body)
	}
	var tpl Template
	if err := json.NewDecoder(resp.Body).Decode(&tpl); err != nil {
		return nil, fmt.Errorf("decode prompt response: %w", err)
	}
	if tpl.Name == "" {
		tpl.Name = name
	}
	return &tpl, nil
}
