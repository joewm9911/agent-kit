// fetch.go:外部 skillpack 的拉取与本地化(vendoring)。
//
// ref 三种形态(见 docs/skillpack-design.md §3):
//
//	github.com/<owner>/<repo>[/<subdir>]@<tag|sha>   经 codeload zip 拉取
//	https://<host>/<path>.zip                        直链归档(需 integrity 或 allow_unpinned)
//	file:<path>                                      本地目录(开发/私有分发,每次重物化)
//
// 产物落 <root>/.skills 形态的目录树 + skills.lock(供给链事实):
// 启动期命中且树哈希与 lock 相符 → 零网络;缺失按策略补拉;哈希不符
// fail fast,永不静默重下。只用 stdlib,仅装配期可达(运行期零网络)。
package skill

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PackSpec 是一条外部引用(config 的 use: 条目解析后)。
type PackSpec struct {
	Use       string // ref 字符串
	Integrity string // "sha256:<hex>",可选:强校验归档字节
	Name      string // 本地覆盖 "ns/name",可空(取 frontmatter)
}

// PackOptions 是获取策略(app 级 skillpacks 块)。
type PackOptions struct {
	RequireLocal  bool // true:缺失即错(为打包期物化预留的收紧档)
	AllowUnpinned bool // true:放行未 pin 的 ref(lock 仍会锁死首次解析结果)
}

// PackDir 是一个已物化并通过校验的包。
type PackDir struct {
	Dir     string // 本地目录(.skills/<ns>/<name>@<version>)
	Ref     string
	NS      string
	Name    string
	Version string
	SHA     string // 内容树哈希(sha256)
}

// packRef 是解析后的引用。
type packRef struct {
	raw     string
	kind    string // file | archive
	url     string // archive 下载地址
	subdir  string // 归档内子路径(github 短写的 /<subdir>)
	path    string // file: 的本地路径
	version string // @后缀;可空
	pinned  bool
}

// parsePackRef 解析 ref 字符串。pin 判定:file: 恒 pin(本地开发形态);
// github 短写必须带 @;https 直链本身无版本概念,带 integrity 才算 pin。
func parsePackRef(raw, integrity string) (packRef, error) {
	switch {
	case strings.HasPrefix(raw, "file:"):
		p := strings.TrimPrefix(raw, "file:")
		if p == "" {
			return packRef{}, fmt.Errorf("skillpack: empty file ref")
		}
		return packRef{raw: raw, kind: "file", path: p, pinned: true}, nil

	case strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "http://127.0.0.1"), strings.HasPrefix(raw, "http://localhost"):
		// 明文 http 只放行本机回环(测试/本地私有分发),公网必须 https。
		if !strings.HasSuffix(raw, ".zip") {
			return packRef{}, fmt.Errorf("skillpack: https ref must point to a .zip archive: %s", raw)
		}
		return packRef{raw: raw, kind: "archive", url: raw, pinned: integrity != ""}, nil

	case strings.HasPrefix(raw, "github.com/"):
		spec, ver, hasVer := strings.Cut(raw, "@")
		parts := strings.Split(strings.TrimPrefix(spec, "github.com/"), "/")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return packRef{}, fmt.Errorf("skillpack: bad github ref %q, want github.com/<owner>/<repo>[/<subdir>]@<tag|sha>", raw)
		}
		owner, repo := parts[0], parts[1]
		sub := strings.Join(parts[2:], "/")
		return packRef{
			raw: raw, kind: "archive",
			url:    fmt.Sprintf("https://codeload.github.com/%s/%s/zip/%s", owner, repo, ver),
			subdir: sub, version: ver, pinned: hasVer && ver != "",
		}, nil

	default:
		return packRef{}, fmt.Errorf("skillpack: unsupported ref %q (want github.com/... | https://...zip | file:...)", raw)
	}
}

// ---- skills.lock ----

type lockFile struct {
	Packs []lockEntry `yaml:"packs"`
}

type lockEntry struct {
	Ref      string `yaml:"ref"`
	Name     string `yaml:"name"` // 最终 ns/name
	Version  string `yaml:"version"`
	SHA256   string `yaml:"sha256"` // 解包后内容树哈希
	SyncedAt string `yaml:"synced_at"`
}

func lockPath(root string) string { return filepath.Join(root, "skills.lock") }

func readLock(root string) (*lockFile, error) {
	raw, err := os.ReadFile(lockPath(root))
	if os.IsNotExist(err) {
		return &lockFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var lf lockFile
	if err := yaml.Unmarshal(raw, &lf); err != nil {
		return nil, fmt.Errorf("skillpack: parse %s: %w", lockPath(root), err)
	}
	return &lf, nil
}

func (lf *lockFile) find(ref string) *lockEntry {
	for i := range lf.Packs {
		if lf.Packs[i].Ref == ref {
			return &lf.Packs[i]
		}
	}
	return nil
}

func (lf *lockFile) upsert(e lockEntry) {
	if cur := lf.find(e.Ref); cur != nil {
		*cur = e
		return
	}
	lf.Packs = append(lf.Packs, e)
}

func writeLock(root string, lf *lockFile) error {
	sort.Slice(lf.Packs, func(i, j int) bool { return lf.Packs[i].Ref < lf.Packs[j].Ref })
	raw, err := yaml.Marshal(lf)
	if err != nil {
		return err
	}
	header := "# skills.lock:外部 skillpack 的供给链事实(ref → 版本 + 内容树哈希)。\n# 由装配期生成,建议提交版本库(与 go.sum 同待遇);哈希不符即 fail fast。\n"
	return os.WriteFile(lockPath(root), append([]byte(header), raw...), 0o644)
}

// ---- EnsurePack:命中即用,缺失按策略补拉 ----

// EnsurePack 保证 spec 引用的包物化在 root 下并通过校验。
//
//	lock 命中且目录树哈希相符 → 直接返回(零网络);
//	缺失:RequireLocal → 错;否则拉取,与 lock(若有)比对后落盘;
//	哈希不符 → 错(篡改/漂移,永不静默重下)。
//
// file: 引用是本地开发形态,每次重物化并刷新 lock(漂移是预期)。
func EnsurePack(ctx context.Context, root string, spec PackSpec, opts PackOptions) (PackDir, error) {
	ref, err := parsePackRef(spec.Use, spec.Integrity)
	if err != nil {
		return PackDir{}, err
	}
	if !ref.pinned && !opts.AllowUnpinned {
		return PackDir{}, fmt.Errorf("skillpack %s: ref is not version-pinned (github needs @tag|@sha, https needs integrity); set allow_unpinned: true explicitly if drift is intended", spec.Use)
	}
	lf, err := readLock(root)
	if err != nil {
		return PackDir{}, err
	}
	entry := lf.find(spec.Use)

	// 命中路径:lock 有记录、目录在、哈希符。file: 跳过(总是重物化)。
	if entry != nil && ref.kind != "file" {
		dir := packDirPath(root, entry.Name, entry.Version)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			sha, err := treeHash(dir)
			if err != nil {
				return PackDir{}, err
			}
			if sha != entry.SHA256 {
				return PackDir{}, fmt.Errorf("skillpack %s: local content does not match skills.lock (tampering or drift)\n  dir: %s\n  want: %s\n  got: %s\n  to accept the change, delete this directory and the lock entry, then re-assemble", spec.Use, dir, entry.SHA256, sha)
			}
			ns, name, _ := splitPackName(entry.Name)
			return PackDir{Dir: dir, Ref: spec.Use, NS: ns, Name: name, Version: entry.Version, SHA: sha}, nil
		}
	}

	if opts.RequireLocal && ref.kind != "file" {
		return PackDir{}, fmt.Errorf("skillpack %s: not materialized locally and sync: require-local (run one assembly in a networked environment first, or run the build-time sync)", spec.Use)
	}

	// 拉取到临时目录。
	tmp, err := os.MkdirTemp(root, ".fetch-*")
	if err != nil {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return PackDir{}, err
		}
		if tmp, err = os.MkdirTemp(root, ".fetch-*"); err != nil {
			return PackDir{}, err
		}
	}
	defer os.RemoveAll(tmp)

	var srcDir string
	switch ref.kind {
	case "file":
		srcDir = ref.path
		if !filepath.IsAbs(srcDir) {
			srcDir = filepath.Clean(srcDir)
		}
	case "archive":
		raw, err := download(ctx, ref.url)
		if err != nil {
			return PackDir{}, fmt.Errorf("skillpack %s: %w", spec.Use, err)
		}
		if spec.Integrity != "" {
			if err := verifyIntegrity(raw, spec.Integrity); err != nil {
				return PackDir{}, fmt.Errorf("skillpack %s: %w", spec.Use, err)
			}
		}
		if srcDir, err = unzip(raw, tmp, ref.subdir); err != nil {
			return PackDir{}, fmt.Errorf("skillpack %s: %w", spec.Use, err)
		}
	}

	// 包根必须有 SKILL.md;读 frontmatter 定名(spec.Name 覆盖优先)。
	fullName := spec.Name
	if fullName == "" {
		name, _, _, _, _, err := parseSkillMD(filepath.Join(srcDir, "SKILL.md"))
		if err != nil {
			return PackDir{}, fmt.Errorf("skillpack %s: %w", spec.Use, err)
		}
		fullName = name
	}
	ns, name, err := splitPackName(fullName)
	if err != nil {
		return PackDir{}, err
	}

	// 物化:先复制成规整树再算哈希(排除 VCS 噪声由复制规则保证)。
	staged := filepath.Join(tmp, "staged")
	if err := copyTree(srcDir, staged); err != nil {
		return PackDir{}, err
	}
	sha, err := treeHash(staged)
	if err != nil {
		return PackDir{}, err
	}
	version := ref.version
	if version == "" {
		version = sha[:12] // 无版本形态(file:/直链):内容即版本
	}
	// 与既有 lock 比对:重物化必须复现同一内容(file: 例外,漂移是预期)。
	if entry != nil && ref.kind != "file" && entry.SHA256 != sha {
		return PackDir{}, fmt.Errorf("skillpack %s: re-fetched content does not match skills.lock (upstream drift)\n  want: %s\n  got: %s\n  to accept, delete the lock entry and re-assemble", spec.Use, entry.SHA256, sha)
	}

	final := packDirPath(root, ns+"/"+name, version)
	if err := os.RemoveAll(final); err != nil {
		return PackDir{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return PackDir{}, err
	}
	if err := os.Rename(staged, final); err != nil {
		return PackDir{}, err
	}
	lf.upsert(lockEntry{
		Ref: spec.Use, Name: ns + "/" + name, Version: version, SHA256: sha,
		SyncedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err := writeLock(root, lf); err != nil {
		return PackDir{}, err
	}
	return PackDir{Dir: final, Ref: spec.Use, NS: ns, Name: name, Version: version, SHA: sha}, nil
}

func packDirPath(root, fullName, version string) string {
	return filepath.Join(root, filepath.FromSlash(fullName)+"@"+version)
}

// splitPackName 拆 "ns/name";裸名默认 ns=pack(标识外部供给来源)。
func splitPackName(full string) (ns, name string, err error) {
	if full == "" {
		return "", "", fmt.Errorf("skillpack: SKILL.md frontmatter is missing name and no local override was provided")
	}
	if !strings.Contains(full, "/") {
		return "pack", full, nil
	}
	i := strings.LastIndex(full, "/")
	ns, name = full[:i], full[i+1:]
	if ns == "" || name == "" {
		return "", "", fmt.Errorf("skillpack: bad name %q, want ns/name", full)
	}
	return ns, name, nil
}

// ---- 底层:下载/校验/解包/复制/树哈希 ----

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s → HTTP %d", url, resp.StatusCode)
	}
	const maxArchive = 128 << 20 // 128MB 上限,防拉爆
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxArchive {
		return nil, fmt.Errorf("archive exceeds the %dMB limit", maxArchive>>20)
	}
	return raw, nil
}

func verifyIntegrity(raw []byte, integrity string) error {
	want, ok := strings.CutPrefix(integrity, "sha256:")
	if !ok {
		return fmt.Errorf("integrity only supports sha256:<hex>, got %q", integrity)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("integrity check failed\n  want: sha256:%s\n  got: sha256:%s", want, got)
	}
	return nil
}

// unzip 解包到 dst 并返回包根:github/codeload 归档带单层顶目录,自动下钻;
// 再按 subdir 下钻。zip 条目做路径囚笼(拒绝 ../ 逃逸)。
func unzip(raw []byte, dst, subdir string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("unzip failed: %w", err)
	}
	out := filepath.Join(dst, "unzipped")
	for _, f := range zr.File {
		rel := filepath.Clean(filepath.FromSlash(f.Name))
		if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return "", fmt.Errorf("archive contains an illegal path %q", f.Name)
		}
		target := filepath.Join(out, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return "", err
		}
	}
	root := out
	// 单层顶目录(codeload 形态)自动下钻。
	if ents, err := os.ReadDir(root); err == nil && len(ents) == 1 && ents[0].IsDir() {
		if _, err := os.Stat(filepath.Join(root, "SKILL.md")); os.IsNotExist(err) {
			root = filepath.Join(root, ents[0].Name())
		}
	}
	if subdir != "" {
		root = filepath.Join(root, filepath.FromSlash(subdir))
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("pack directory %q not found in the archive", subdir)
	}
	return root, nil
}

// copyTree 复制目录树(跳过 VCS/隐藏目录,拒绝符号链接——包内容必须自含)。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		base := filepath.Base(p)
		if info.IsDir() {
			if base == ".git" || base == ".skills" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skillpack: symlinks are not allowed inside a pack: %s", rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, info.Mode().Perm()|0o400)
	})
}

// treeHash 计算目录内容树哈希:按排序后的相对路径,逐文件
// hash(path) + hash(content) 聚合,与平台/时间戳无关。
func treeHash(dir string) (string, error) {
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\x00%x\x00", filepath.ToSlash(rel), sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
