package resource_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/joewm9911/agent-kit/protocol/resource"
)

func read(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return string(b)
}

// TestFileScheme: a bare path roots the FS at its directory and returns
// the base name — relative refs inside resolve against that dir, not CWD.
func TestFileScheme(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("name: a"), 0o644)
	os.MkdirAll(filepath.Join(dir, "prompts"), 0o755)
	os.WriteFile(filepath.Join(dir, "prompts", "p.md"), []byte("hi"), 0o644)

	root, entry, err := resource.Resolve(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if entry != "app.yaml" {
		t.Fatalf("entry = %q", entry)
	}
	if got := read(t, root, entry); got != "name: a" {
		t.Fatalf("app = %q", got)
	}
	// A sibling reference resolves within the same root, regardless of CWD.
	if got := read(t, root, "prompts/p.md"); got != "hi" {
		t.Fatalf("prompt = %q", got)
	}
}

// TestEmbedScheme: a registered FS is referenced as embed:<name>/<entry>.
func TestEmbedScheme(t *testing.T) {
	resource.RegisterEmbed("t1", fstest.MapFS{
		"config/app.yaml":     {Data: []byte("name: e")},
		"config/prompts/p.md": {Data: []byte("emb")},
	})
	root, entry, err := resource.Resolve("embed:t1/config/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if entry != "config/app.yaml" || read(t, root, entry) != "name: e" {
		t.Fatalf("entry=%q content mismatch", entry)
	}
	if read(t, root, "config/prompts/p.md") != "emb" {
		t.Fatal("sibling in embed not reachable")
	}
}

// TestSchemeFailFast: unknown scheme, unregistered embed, and malformed
// embed ref all fail fast with a directive error.
func TestSchemeFailFast(t *testing.T) {
	if _, _, err := resource.Resolve("weird:foo"); err == nil || !strings.Contains(err.Error(), "unknown scheme") {
		t.Fatalf("unknown scheme must fail, got %v", err)
	}
	if _, _, err := resource.Resolve("embed:nope/x.yaml"); err == nil || !strings.Contains(err.Error(), "RegisterEmbed") {
		t.Fatalf("unregistered embed must point to RegisterEmbed, got %v", err)
	}
	if _, _, err := resource.Resolve("embed:justname"); err == nil || !strings.Contains(err.Error(), "embed:<name>/<entry>") {
		t.Fatalf("malformed embed ref must fail, got %v", err)
	}
}

// TestSplitSchemeDriveLetter: a bare path (incl. a Windows-style drive
// letter) is not mistaken for a scheme.
func TestBarePathNotScheme(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x"), 0o644)
	// Absolute unix path has no scheme.
	if _, entry, err := resource.Resolve(filepath.Join(dir, "a.yaml")); err != nil || entry != "a.yaml" {
		t.Fatalf("bare path misparsed: entry=%q err=%v", entry, err)
	}
}

// TestFind: explicit env wins; otherwise an existing relative path is
// returned; a missing name errors with where it looked.
func TestFind(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "app.yaml")
	os.WriteFile(cfg, []byte("x"), 0o644)

	t.Setenv("AGENTKIT_CONFIG", cfg)
	if got, err := resource.Find("ignored.yaml"); err != nil || got != cfg {
		t.Fatalf("env should win: got %q err %v", got, err)
	}

	t.Setenv("AGENTKIT_CONFIG", "")
	if got, err := resource.Find(cfg); err != nil || got != cfg {
		t.Fatalf("existing abs path: got %q err %v", got, err)
	}
	if _, err := resource.Find("definitely-missing-xyz.yaml"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing config must fail with search list, got %v", err)
	}
	// A scheme ref passes through Find untouched.
	if got, err := resource.Find("embed:main/app.yaml"); err != nil || got != "embed:main/app.yaml" {
		t.Fatalf("scheme ref should pass through: got %q err %v", got, err)
	}
}
