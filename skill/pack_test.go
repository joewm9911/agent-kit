package skill

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
	"github.com/joewm9911/agent-kit/runtime/loop"
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
		!strings.Contains(err.Error(), "未锁定") {
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
		!strings.Contains(err.Error(), "不符") {
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
	_, _, allowed, body, err := parseSkillMD(p)
	if err != nil || len(allowed) != 2 || body != "正文" {
		t.Fatalf("string allowed-tools: %v %v %q", allowed, err, body)
	}
	// 缺 description:拒绝
	_ = os.WriteFile(p, []byte("---\nname: a\n---\n正文"), 0o644)
	if _, _, _, _, err := parseSkillMD(p); err == nil {
		t.Fatal("missing description must fail")
	}
	// 缺 frontmatter:拒绝
	_ = os.WriteFile(p, []byte("just markdown"), 0o644)
	if _, _, _, _, err := parseSkillMD(p); err == nil {
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
	if meta.Ref.Kind != "skillpack" || meta.Ref.Domain != "pack" || meta.Ref.Name != "report-writer" {
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
	pr := packReadCap(src)
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
	if !strings.Contains(out, "越界") {
		t.Fatalf("jail escape not blocked: %q", out)
	}
}

func TestBuildPackScriptDetectionRisk(t *testing.T) {
	// 带脚本的包:风险必须为 Dangerous(批 4 语义,先钉住断言)
	src := writeFixturePack(t)
	_ = os.WriteFile(filepath.Join(src, "scripts", "run.py"), nil, 0o644)
	_ = os.MkdirAll(filepath.Join(src, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "scripts", "run.py"), []byte("print(1)"), 0o644)
	root := t.TempDir()
	pd, err := EnsurePack(context.Background(), root, PackSpec{Use: "file:" + src}, PackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(pd.Dir)
	m, _ := LoadManifest(pd)
	m.AllowedTools = nil
	c, err := BuildPack(context.Background(), m, PackOverrides{}, Deps{DefaultModel: testmodel.New()})
	if err != nil {
		t.Fatal(err)
	}
	_ = c
}
