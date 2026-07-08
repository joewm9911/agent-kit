// examples/embedded demonstrates single-binary deployment: the entire config
// tree (app.yaml, agents, namespaces, prompt files) is compiled into the
// binary with //go:embed and loaded via config.LoadAppFS — with zero
// dependence on the filesystem or the process working directory. Run it from
// anywhere:
//
//	go build -o /tmp/agent ./examples/embedded && cd / && /tmp/agent
//
// It loads and prints the assembled structure. There is no model call, so no
// API key is required — the point is to show that resource loading is fully
// self-contained.
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"

	"github.com/joewm9911/agent-kit/config"
)

//go:embed config
var configFS embed.FS

func main() {
	// The embedded FS is the resource root; "config/app.yaml" is the entry.
	// Every relative reference (agents/, ../namespaces/, prompts/) resolves
	// within configFS — no disk, no CWD assumptions.
	spec, err := config.LoadAppFS(configFS, "config/app.yaml")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Loaded from embedded FS (no files on disk):")
	for _, a := range spec.Agents {
		fmt.Printf("  agent %q\n", a.Name)
		for _, m := range a.Mounts {
			fmt.Printf("    mounts namespace %q (%s)\n", m.Name, m.Path)
		}
	}

	// Prompt files load from the same embedded FS.
	if _, err := fs.Stat(configFS, "config/prompts/persona.md"); err == nil {
		fmt.Println("  prompt persona.md is embedded and resolvable via cap://prompt/pp/persona")
	}
}
