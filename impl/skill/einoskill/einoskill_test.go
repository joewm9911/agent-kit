package einoskill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePack 造一个物化布局的技能包目录:<root>/<ns>/<name@ver>/SKILL.md。
func writePack(t *testing.T, root, ns, dir, front, body string) {
	t.Helper()
	d := filepath.Join(root, ns, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\n" + front + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBackendListGet 验证适配器:List 输出与目录内容一致(含 frontmatter
// 扩展字段透传),Get 按 <ns>/<name> 取回正文与绝对路径。
func TestBackendListGet(t *testing.T) {
	root := t.TempDir()
	writePack(t, root, "docs", "pdf@abc123",
		"name: pdf\ndescription: 处理 PDF\ncontext: fork_with_context\nmodel: fast", "用脚本处理 PDF。")
	writePack(t, root, "dev", "review",
		"name: review\ndescription: 代码评审\nagent: reviewer", "按清单评审。")
	// 无 SKILL.md 的目录忽略
	_ = os.MkdirAll(filepath.Join(root, "docs", "junk"), 0o755)

	b := NewBackend(root)
	fms, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fms) != 2 {
		t.Fatalf("list = %d, want 2", len(fms))
	}
	byName := map[string]int{}
	for i, fm := range fms {
		byName[fm.Name] = i
	}
	pdf := fms[byName["docs/pdf"]]
	if pdf.Description != "处理 PDF" || string(pdf.Context) != "fork_with_context" || pdf.Model != "fast" {
		t.Fatalf("pdf frontmatter: %+v", pdf)
	}
	if rev := fms[byName["dev/review"]]; rev.Agent != "reviewer" {
		t.Fatalf("review frontmatter: %+v", rev)
	}

	sk, err := b.Get(context.Background(), "docs/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Content != "用脚本处理 PDF。" || !filepath.IsAbs(sk.BaseDirectory) {
		t.Fatalf("get: %+v", sk)
	}
	if !strings.HasSuffix(sk.BaseDirectory, filepath.Join("docs", "pdf@abc123")) {
		t.Fatalf("base dir: %s", sk.BaseDirectory)
	}

	if _, err := b.Get(context.Background(), "nope/x"); err == nil {
		t.Fatal("unknown skill must error")
	}
}
