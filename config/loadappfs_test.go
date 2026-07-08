package config

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestLoadAppFS: a multi-file app loads from an in-memory FS with the entry
// nested under a subdirectory (the embed.FS shape). Agent and namespace
// relative references resolve within the FS root, with zero dependence on
// the process working directory.
func TestLoadAppFS(t *testing.T) {
	fsys := fstest.MapFS{
		"cfg/app.yaml": {Data: []byte(`
secrets: {provider: env}
agents:
  - agents/ops.yaml
`)},
		"cfg/agents/ops.yaml": {Data: []byte(`
name: ops
namespaces:
  - ../namespaces/catalog.yaml
`)},
		"cfg/namespaces/catalog.yaml": {Data: []byte(`
name: catalog
`)},
	}

	spec, err := LoadAppFS(fsys, "cfg/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Root == nil {
		t.Fatal("spec.Root should carry the resource FS")
	}
	if len(spec.Agents) != 1 || spec.Agents[0].Name != "ops" {
		t.Fatalf("agents = %+v", spec.Agents)
	}
	mounts := spec.Agents[0].Mounts
	if len(mounts) != 1 || mounts[0].Name != "catalog" {
		t.Fatalf("mounts = %+v", mounts)
	}
	// namespace path resolved within the FS root (../ collapsed against cfg/agents)
	if mounts[0].Path != "cfg/namespaces/catalog.yaml" {
		t.Fatalf("ns fs path = %q", mounts[0].Path)
	}
}

// TestLoadAppFSMissing: a missing agent file fails with the fs-internal path.
func TestLoadAppFSMissing(t *testing.T) {
	fsys := fstest.MapFS{
		"app.yaml": {Data: []byte("secrets: {provider: env}\nagents:\n  - agents/nope.yaml\n")},
	}
	if _, err := LoadAppFS(fsys, "app.yaml"); err == nil {
		t.Fatal("missing agent file must fail")
	}
}

// TestWorkDirRejected: the removed work_dir key fails fast at build, pointing
// to state_dir (read-only vs writable split).
func TestWorkDirRejected(t *testing.T) {
	legacy := "/tmp/x"
	cfg := &Config{WorkDirLegacy: &legacy}
	_, err := Build(nil, cfg, BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Fatalf("work_dir must fail fast pointing to state_dir, got %v", err)
	}
}
