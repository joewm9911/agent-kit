package prompt

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

// TestFileProviderFS: NewFileProvider serves <name>.md from an fs.FS subtree
// (embed/remote/local all satisfy fs.FS), and rejects '..' traversal out of
// the subtree — the fs.FS path constraint is a free sandbox.
func TestFileProviderFS(t *testing.T) {
	p := NewFileProvider(fstest.MapFS{
		"persona.md":    {Data: []byte("you are ops")},
		"sub/nested.md": {Data: []byte("nested")},
		"../secret.md":  {Data: []byte("should be unreachable via ..")},
	})
	ctx := context.Background()

	tpl, err := p.Get(ctx, "persona", "")
	if err != nil || tpl.Text != "you are ops" {
		t.Fatalf("persona: %v %+v", err, tpl)
	}
	if tpl.Version == "" {
		t.Fatal("version (content hash) should be set")
	}
	if n, err := p.Get(ctx, "sub/nested", ""); err != nil || n.Text != "nested" {
		t.Fatalf("nested: %v %+v", err, n)
	}
	// Traversal out of the subtree is rejected by fs.FS path validation.
	if _, err := p.Get(ctx, "../secret", ""); err == nil {
		t.Fatal("'..' traversal must be rejected")
	}
	// Missing file errors, not panics.
	if _, err := p.Get(ctx, "nope", ""); err == nil || !strings.Contains(err.Error(), "read prompt file") {
		t.Fatalf("missing prompt: %v", err)
	}
}
