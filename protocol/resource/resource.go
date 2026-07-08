// Package resource is the single read-only resource abstraction for the
// framework: configuration (app/agent/namespace), prompt files, secrets
// files, and skill-pack contents all load through an io/fs.FS resolved
// from a resource ref. The FS root is the one anchor for every relative
// reference, so the same configuration loads identically whether it lives
// on local disk, embedded in the binary (embed.FS), or (later) fetched
// from a remote — with no dependence on the process working directory.
//
// A ref selects a scheme:
//
//	./app.yaml, /etc/app/app.yaml   file (default; no scheme prefix)
//	embed:main/config/app.yaml      a host-registered embed.FS
//	https://…                       third-party resolver (not built in)
//
// Third-party sources (OCI, S3, config servers) register a scheme resolver
// that returns an fs.FS — same "code registers, config enables by ref"
// discipline as the rest of the framework.
package resource

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Loader is any read-only resource tree. os.DirFS, embed.FS, and an
// in-memory FS all satisfy it, so consumers take a plain fs.FS.
type Loader = fs.FS

// Resolver turns a scheme-specific ref (the part after "scheme:") into a
// root FS and the entry path within it. The entry path is fs.FS-style:
// '/'-separated, no leading slash, no "..".
type Resolver func(ref string) (fs.FS, string, error)

var (
	mu        sync.RWMutex
	resolvers = map[string]Resolver{}
	embeds    = map[string]fs.FS{}
)

// Register installs a scheme resolver. The "file" scheme (and the
// no-scheme default) and the "embed" scheme are built in; a duplicate
// registration panics (startup-time discipline).
func Register(scheme string, r Resolver) {
	if scheme == "" || r == nil {
		panic("resource: Register requires a non-empty scheme and resolver")
	}
	mu.Lock()
	defer mu.Unlock()
	if scheme == "file" || scheme == "embed" {
		panic(fmt.Sprintf("resource: scheme %q is built in", scheme))
	}
	if _, dup := resolvers[scheme]; dup {
		panic(fmt.Sprintf("resource: scheme %q registered more than once", scheme))
	}
	resolvers[scheme] = r
}

// RegisterEmbed registers a named FS (typically an embed.FS) under the
// "embed" scheme, referenced as "embed:<name>/<entry>". Register during
// host startup, before loading; a duplicate name panics.
func RegisterEmbed(name string, fsys fs.FS) {
	if name == "" || fsys == nil {
		panic("resource: RegisterEmbed requires a non-empty name and fs")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := embeds[name]; dup {
		panic(fmt.Sprintf("resource: embed %q registered more than once", name))
	}
	embeds[name] = fsys
}

// Resolve parses a ref into (root FS, entry path). A ref with no scheme
// prefix is a local file path.
func Resolve(ref string) (fs.FS, string, error) {
	scheme, rest := splitScheme(ref)
	switch scheme {
	case "", "file":
		return fileResolve(rest)
	case "embed":
		return embedResolve(rest)
	}
	mu.RLock()
	r, ok := resolvers[scheme]
	mu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("resource: unknown scheme %q (register a resolver with resource.Register, or pass a file path; registered: %s)", scheme, registeredSchemes())
	}
	return r(rest)
}

// fileResolve roots os.DirFS at the file's directory and returns its base
// name as the entry — so relative references inside the config resolve
// against that directory, not the process working directory.
func fileResolve(p string) (fs.FS, string, error) {
	if p == "" {
		return nil, "", fmt.Errorf("resource: empty file ref")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, "", err
	}
	dir, base := filepath.Split(abs)
	return os.DirFS(dir), base, nil
}

// embedResolve splits "name/entry/path" into the registered FS name and
// the entry path within it.
func embedResolve(rest string) (fs.FS, string, error) {
	name, entry, ok := strings.Cut(rest, "/")
	if !ok || name == "" || entry == "" {
		return nil, "", fmt.Errorf("resource: embed ref must be embed:<name>/<entry>, got %q", rest)
	}
	mu.RLock()
	fsys, found := embeds[name]
	mu.RUnlock()
	if !found {
		return nil, "", fmt.Errorf("resource: embed %q is not registered (call resource.RegisterEmbed(%q, <embed.FS>) during startup)", name, name)
	}
	return fsys, path.Clean(entry), nil
}

// splitScheme separates a "scheme:rest" prefix from a ref. A scheme is a
// lowercase-alphabetic token of length >= 2 (so a Windows drive letter
// "C:" is not mistaken for a scheme, and bare paths pass through).
func splitScheme(ref string) (scheme, rest string) {
	i := strings.IndexByte(ref, ':')
	if i < 2 {
		return "", ref
	}
	s := ref[:i]
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return "", ref
		}
	}
	return s, ref[i+1:]
}

func registeredSchemes() string {
	mu.RLock()
	defer mu.RUnlock()
	out := []string{"file", "embed"}
	for s := range resolvers {
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// configEnv is the environment variable that pins the config ref explicitly.
const configEnv = "AGENTKIT_CONFIG"

// Find locates a config ref for loading. Precedence:
//
//  1. $AGENTKIT_CONFIG (if set) — absolute, scheme, or relative;
//  2. the given ref as provided;
//  3. the ref resolved against the executable's directory;
//  4. the ref resolved against /etc/agentkit.
//
// A scheme ref (embed:…, https:…) or an existing path is returned as-is.
// Otherwise Find returns the first candidate that exists on disk, or an
// error listing where it looked. Use Find for a bare/relative name; pass
// an already-resolved ref straight to Resolve.
func Find(ref string) (string, error) {
	if env := os.Getenv(configEnv); env != "" {
		ref = env
	}
	if scheme, _ := splitScheme(ref); scheme != "" && scheme != "file" {
		return ref, nil // embed:/https: etc. — not a filesystem path
	}
	if filepath.IsAbs(ref) {
		return ref, nil
	}
	var tried []string
	candidates := []string{ref}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ref))
	}
	candidates = append(candidates, filepath.Join("/etc/agentkit", ref))
	for _, c := range candidates {
		tried = append(tried, c)
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("resource: config %q not found (set %s, or place it in one of: %s)", ref, configEnv, strings.Join(tried, ", "))
}
