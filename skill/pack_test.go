package skill

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/protocol/source"
	"github.com/joewm9911/agent-kit/runtime/loop"

	einomodel "github.com/cloudwego/eino/components/model"
	_ "github.com/joewm9911/agent-kit/impl/source/exectool"
)

const fixtureSkillMD = `---
name: report-writer
description: 写一份结构化报告
allowed-tools:
  - "cap://tool/t/search"
---
你是报告写手[PACKBODY]。先检索再成文,结论先行。
`

// writeFixturePack 造一个本地包目录:SKILL.md + 一个附属文件。
func writeFixturePack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(fixtureSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "outline.md"), []byte("# 大纲模板"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// zipFixture 把目录打成 zip 字节(模拟远端归档)。
func zipFixture(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, _ := os.ReadFile(p)
		_, err = w.Write(data)
		return err
	})
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParsePackRef(t *testing.T) {
	r, err := parsePackRef("github.com/o/r/skills/pdf@v1.2", "")
	if err != nil || !r.pinned || r.subdir != "skills/pdf" || r.version != "v1.2" ||
		r.url != "https://codeload.github.com/o/r/zip/v1.2" {
		t.Fatalf("github ref: %+v %v", r, err)
	}
	if r, _ := parsePackRef("github.com/o/r", ""); r.pinned {
		t.Fatal("github without @ must be unpinned")
	}
	if r, _ := parsePackRef("https://x.com/p.zip", ""); r.pinned {
		t.Fatal("bare https must be unpinned")
	}
	if r, _ := parsePackRef("https://x.com/p.zip", "sha256:ab"); !r.pinned {
		t.Fatal("https with integrity is pinned")
	}
	if r, _ := parsePackRef("file:./pack", ""); !r.pinned || r.kind != "file" {
		t.Fatal("file ref must be pinned")
	}
	for _, bad := range []string{"ftp://x", "github.com/only-owner@v1", "https://x.com/p.tar.bz2"} {
		if _, err := parsePackRef(bad, ""); err == nil {
			t.Fatalf("expect error for %q", bad)
		}
	}
}

func TestEnsurePackFromFileAndLock(t *testing.T) {
	src := writeFixturePack(t)
	root := t.TempDir()
	pd, err := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pd.NS != "pack" || pd.Name != "report-writer" {
		t.Fatalf("name from frontmatter: %+v", pd)
	}
	if _, err := os.Stat(filepath.Join(pd.Dir, "templates", "outline.md")); err != nil {
		t.Fatalf("materialized tree incomplete: %v", err)
	}
	lf, _ := readLock(root)
	if e := lf.find("file:" + src); e == nil || e.SHA256 != pd.SHA {
		t.Fatalf("lock not written: %+v", lf)
	}
	// name 覆盖
	pd2, err := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src, Name: "docs/writer"}, PackOptions{})
	if err != nil || pd2.NS != "docs" || pd2.Name != "writer" {
		t.Fatalf("override name: %+v %v", pd2, err)
	}
}

func TestEnsurePackHTTPSPinnedByIntegrity(t *testing.T) {
	src := writeFixturePack(t)
	raw := zipFixture(t, src)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	url := srv.URL + "/pack.zip"
	sum := sha256.Sum256(raw)
	integrity := "sha256:" + hex.EncodeToString(sum[:])
	root := t.TempDir()

	// 未 pin:默认拒绝
	if _, err := EnsurePack(context.Background(), root, PackSpec{Use: url}, PackOptions{}); err == nil ||
		!strings.Contains(err.Error(), "not version-pinned") {
		t.Fatalf("unpinned must fail fast, got %v", err)
	}
	// allow_unpinned:放行且 lock 锁死
	pd, err := EnsurePack(context.Background(), root, PackSpec{Use: url}, PackOptions{AllowUnpinned: true})
	if err != nil {
		t.Fatal(err)
	}
	// integrity 正确:pin 放行
	root2 := t.TempDir()
	if _, err := EnsurePack(context.Background(), root2, PackSpec{Use: url, Integrity: integrity}, PackOptions{}); err != nil {
		t.Fatal(err)
	}
	// integrity 错误:拒绝
	root3 := t.TempDir()
	if _, err := EnsurePack(context.Background(), root3, PackSpec{Use: url, Integrity: "sha256:" + strings.Repeat("0", 64)}, PackOptions{}); err == nil ||
		!strings.Contains(err.Error(), "integrity") {
		t.Fatalf("bad integrity must fail, got %v", err)
	}

	// 命中缓存:关掉服务器仍可装配(零网络)
	srv.Close()
	pdc, err := EnsurePack(context.Background(), root, PackSpec{Use: url}, PackOptions{AllowUnpinned: true})
	if err != nil || pdc.SHA != pd.SHA {
		t.Fatalf("cache hit should not touch network: %v", err)
	}

	// 篡改本地内容:哈希不符 fail fast
	if err := os.WriteFile(filepath.Join(pd.Dir, "SKILL.md"), []byte("---\nname: x\ndescription: y\n---\n改"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePack(context.Background(), root, PackSpec{Use: url}, PackOptions{AllowUnpinned: true}); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tamper must fail fast, got %v", err)
	}
}

func TestEnsurePackRequireLocal(t *testing.T) {
	root := t.TempDir()
	_, err := EnsurePack(context.Background(), root,
		PackSpec{Use: "https://unreachable.invalid/p.zip", Integrity: "sha256:" + strings.Repeat("a", 64)},
		PackOptions{RequireLocal: true})
	if err == nil || !strings.Contains(err.Error(), "require-local") {
		t.Fatalf("require-local missing must fail fast, got %v", err)
	}
}

func TestParseSkillMDVariants(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	// 字符串形态 allowed-tools(Claude Code 生态常见)
	_ = os.WriteFile(p, []byte("---\nname: a\ndescription: d\nallowed-tools: \"cap://tool/x/a, cap://tool/x/b\"\n---\n正文"), 0o644)
	_, _, allowed, _, body, err := parseSkillMD(p)
	if err != nil || len(allowed) != 2 || body != "正文" {
		t.Fatalf("string allowed-tools: %v %v %q", allowed, err, body)
	}
	// 缺 description:拒绝
	_ = os.WriteFile(p, []byte("---\nname: a\n---\n正文"), 0o644)
	if _, _, _, _, _, err := parseSkillMD(p); err == nil {
		t.Fatal("missing description must fail")
	}
	// 缺 frontmatter:拒绝
	_ = os.WriteFile(p, []byte("just markdown"), 0o644)
	if _, _, _, _, _, err := parseSkillMD(p); err == nil {
		t.Fatal("missing frontmatter must fail")
	}
}

// fakeSelector:最小目录,按精确引用给工具。
type fakeSelector struct {
	caps map[string]capability.Capability
}

func (f fakeSelector) Select(include, _ []string) ([]capability.Capability, error) {
	var out []capability.Capability
	for _, ref := range include {
		if c, ok := f.caps[ref]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestBuildPackInstructionOnly(t *testing.T) {
	src := writeFixturePack(t)
	root := t.TempDir()
	pd, err := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(pd)
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasFiles || len(m.AllowedTools) != 1 || !strings.Contains(m.Body, "[PACKBODY]") {
		t.Fatalf("manifest: %+v", m)
	}

	search := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Domain: "t", Name: "search"},
	}, func(context.Context, string) (string, error) { return "检索结果", nil })

	em := &echoInputModel{}
	cap0, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{
		DefaultModel: em,
		Catalog:      fakeSelector{caps: map[string]capability.Capability{"cap://tool/t/search": search}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// L1:kind/ns/名字/描述进目录
	meta := cap0.Meta()
	if meta.Ref.Kind != "skill" || meta.Ref.Domain != "pack" || meta.Ref.Name != "report-writer" {
		t.Fatalf("meta: %+v", meta.Ref)
	}
	// 调用:L2 正文注入子循环 system 层,宿主只拿最终结果
	out, err := capability.Invoke(context.Background(), cap0, `{"input":"写季报"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("empty result")
	}
	sys := em.seen[0][0]
	if sys.Role != schema.System || !strings.Contains(sys.Content, "[PACKBODY]") {
		t.Fatalf("L2 body must land in sub-loop system prompt, got %q", sys.Content)
	}
}

func TestBuildPackForkContext(t *testing.T) {
	src := writeFixturePack(t)
	root := t.TempDir()
	pd, _ := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src}, PackOptions{})
	m, _ := LoadManifest(pd)
	m.AllowedTools = nil // 纯指令形态

	em := &echoInputModel{}
	forked, err := BuildPack(context.Background(), m, PackOverrides{Context: "fork"}, Deps{DefaultModel: em})
	if err != nil {
		t.Fatal(err)
	}
	snap := []*schema.Message{schema.UserMessage("宿主聊过 payment 超时")}
	ctx := loop.WithConversationSnapshot(runctx.With(context.Background(), "host", "s1"), snap)
	if _, err := capability.Invoke(ctx, forked, `{"input":"继续分析"}`); err != nil {
		t.Fatal(err)
	}
	var sawSnap bool
	for _, msg := range em.seen[0] {
		if strings.Contains(msg.Content, "payment") {
			sawSnap = true
		}
	}
	if !sawSnap {
		t.Fatal("context: fork must carry the caller conversation snapshot")
	}
	// 非 fork:快照不得泄入
	em2 := &echoInputModel{}
	fresh, _ := BuildPack(context.Background(), m, PackOverrides{}, Deps{DefaultModel: em2})
	if _, err := capability.Invoke(ctx, fresh, `{"input":"继续分析"}`); err != nil {
		t.Fatal(err)
	}
	for _, msg := range em2.seen[0] {
		if strings.Contains(msg.Content, "payment") {
			t.Fatal("fresh pack must not see caller conversation")
		}
	}
}

func TestPackReadJail(t *testing.T) {
	src := writeFixturePack(t)
	pr := packReadCap(packFS(src))
	ctx := context.Background()
	out, _ := capability.Invoke(ctx, pr, `{}`)
	if !strings.Contains(out, "templates/outline.md") {
		t.Fatalf("list: %q", out)
	}
	out, _ = capability.Invoke(ctx, pr, `{"path":"templates/outline.md"}`)
	if !strings.Contains(out, "大纲模板") {
		t.Fatalf("read: %q", out)
	}
	out, _ = capability.Invoke(ctx, pr, `{"path":"../../etc/passwd"}`)
	if !strings.Contains(out, "out of bounds") {
		t.Fatalf("jail escape not blocked: %q", out)
	}
}

// TestPackReadFS: the fs cap works over any fs.FS (here an in-memory FS,
// standing in for an embedded pack) — deployment-agnostic, same '..' jail.
func TestPackReadFS(t *testing.T) {
	pr := packReadCap(fstest.MapFS{
		"SKILL.md":             {Data: []byte("---\nname: t\n---\nbody")},
		"templates/outline.md": {Data: []byte("outline")},
	})
	ctx := context.Background()
	if out, _ := capability.Invoke(ctx, pr, `{}`); !strings.Contains(out, "templates/outline.md") {
		t.Fatalf("list: %q", out)
	}
	if out, _ := capability.Invoke(ctx, pr, `{"path":"templates/outline.md"}`); out != "outline" {
		t.Fatalf("read: %q", out)
	}
	if out, _ := capability.Invoke(ctx, pr, `{"path":"../escape"}`); !strings.Contains(out, "out of bounds") {
		t.Fatalf("jail escape not blocked: %q", out)
	}
}

func TestScriptPackRuntimesAndExecBinding(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	src := writeFixturePack(t)
	_ = os.MkdirAll(filepath.Join(src, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "scripts", "read.py"), []byte("print(open('data.txt').read())"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "data.txt"), []byte("包内数据123"), 0o644)

	root := t.TempDir()
	pd, err := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(pd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Runtimes) != 1 || m.Runtimes[0] != "python" {
		t.Fatalf("runtimes detection: %v", m.Runtimes)
	}

	// 装配层形态:经 source 注册表构造 workdir 绑定的 exec 工具
	execSrc, err := source.New(context.Background(), "exec", "pack", map[string]any{
		"workdir": m.Dir, "tools": []map[string]any{{"name": "python", "runtime": "python"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	extra, err := execSrc.Sync(context.Background())
	if err != nil || len(extra) != 1 {
		t.Fatalf("exec caps: %v %v", extra, err)
	}
	// 工作目录绑定:脚本以相对路径读到包内文件
	out, err := capability.Invoke(context.Background(), extra[0],
		`{"script":"print(open('data.txt').read())"}`)
	if err != nil || !strings.Contains(out, "包内数据123") {
		t.Fatalf("workdir binding failed: %q %v", out, err)
	}

	// 风险传播:带 exec 工具的包 = Dangerous
	m.AllowedTools = nil
	c, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{DefaultModel: testmodel.New()}, extra...)
	if err != nil {
		t.Fatal(err)
	}
	if c.Meta().Risk != capability.RiskDangerous {
		t.Fatalf("script pack risk = %v, want dangerous", c.Meta().Risk)
	}
}

// writeFrontPack 造一个带扩展 frontmatter 的纯指令包并物化。
func writeFrontPack(t *testing.T, front string) *PackManifest {
	t.Helper()
	dir := t.TempDir()
	md := "---\nname: fx/probe\ndescription: 探针技能\n" + front + "\n---\n[FXBODY] 按指引行事。"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	pd, err := EnsurePack(context.Background(), t.TempDir(), PackSpec{Use: "file:" + dir}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(pd)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestManifestFrontMatterCompat 验证 eino/agentskills 协议字段:context/
// agent/model 解析;非法 context 与 agent+model 同设拒绝。
func TestManifestFrontMatterCompat(t *testing.T) {
	// 旧值兼容映射:fork_with_context(快照)→ fork;fork(eino 隔离)→ fresh
	m := writeFrontPack(t, "context: fork_with_context\nmodel: fast")
	if m.Context != "fork" || m.Model != "fast" || m.Agent != "" {
		t.Fatalf("manifest front: %+v", m)
	}
	if m2 := writeFrontPack(t, "context: fork"); m2.Context != "fresh" {
		t.Fatalf("legacy fork should map to fresh(隔离), got %q", m2.Context)
	}
	if m3 := writeFrontPack(t, "context: fresh"); m3.Context != "fresh" {
		t.Fatalf("fresh: %+v", m3)
	}

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: x\ndescription: d\ncontext: banana\n---\n正文"), 0o644)
	pd, err := EnsurePack(context.Background(), t.TempDir(), PackSpec{Use: "file:" + dir}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(pd); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("invalid context must fail, got %v", err)
	}

	dir2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir2, "SKILL.md"),
		[]byte("---\nname: x\ndescription: d\nagent: a\nmodel: m\n---\n正文"), 0o644)
	pd2, err := EnsurePack(context.Background(), t.TempDir(), PackSpec{Use: "file:" + dir2}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(pd2); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("agent+model must be mutually exclusive, got %v", err)
	}
}

// TestBuildPackModelHub 验证 frontmatter model: 经 ModelHub 解析;
// 缺 Hub fail fast;条目覆盖优先级不受影响(ov.Model 已有既有测试)。
func TestBuildPackModelHub(t *testing.T) {
	m := writeFrontPack(t, "model: fast")

	if _, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{DefaultModel: &echoInputModel{}}); err == nil ||
		!strings.Contains(err.Error(), "ModelHub") {
		t.Fatalf("model: without hub must fail fast, got %v", err)
	}

	hubbed := &echoInputModel{}
	resolved := ""
	deps := Deps{DefaultModel: &echoInputModel{}, ModelHub: func(_ context.Context, name string) (einomodel.ToolCallingChatModel, error) {
		resolved = name
		return hubbed, nil
	}}
	c, err := BuildPack(context.Background(), m, PackOverrides{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capability.Invoke(context.Background(), c, `{"input":"干活"}`); err != nil {
		t.Fatal(err)
	}
	if resolved != "fast" || len(hubbed.seen) == 0 {
		t.Fatalf("hub model must be used: resolved=%q calls=%d", resolved, len(hubbed.seen))
	}
}

// TestBuildPackAgentDelegate 验证 frontmatter agent: 委托:目标经 AgentHub
// 调用期解析,收到 L2 正文 + 任务组成的完整指令;fork_with_context 透传
// 快照 fork 语义;查不到报错;缺 Hub 装配 fail fast。
func TestBuildPackAgentDelegate(t *testing.T) {
	m := writeFrontPack(t, "agent: helper\ncontext: fork_with_context")

	if _, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{DefaultModel: &echoInputModel{}}); err == nil ||
		!strings.Contains(err.Error(), "AgentHub") {
		t.Fatalf("agent: without hub must fail fast, got %v", err)
	}

	var gotArgs string
	var gotFork bool
	target := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "agent", Domain: "app", Name: "helper"},
	}, func(ctx context.Context, argsJSON string) (string, error) {
		gotArgs, gotFork = argsJSON, runctx.ForkRequested(ctx)
		return "delegated-ok", nil
	})
	hub := func(name string) (capability.Capability, bool) {
		if name == "helper" {
			return target, true
		}
		return nil, false
	}
	c, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{AgentHub: hub})
	if err != nil {
		t.Fatal(err)
	}
	out, err := capability.Invoke(context.Background(), c, `{"input":"查一下P100"}`)
	if err != nil || out != "delegated-ok" {
		t.Fatalf("delegate: %q %v", out, err)
	}
	if !strings.Contains(gotArgs, "[FXBODY]") || !strings.Contains(gotArgs, "查一下P100") {
		t.Fatalf("target must receive body+task, got %q", gotArgs)
	}
	if !gotFork {
		t.Fatal("fork_with_context must request snapshot fork")
	}

	// 查不到:调用期报错
	m2 := writeFrontPack(t, "agent: ghost")
	c2, err := BuildPack(context.Background(), m2, PackOverrides{}, Deps{AgentHub: hub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capability.Invoke(context.Background(), c2, `{"input":"x"}`); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown agent must error at call time, got %v", err)
	}
}
