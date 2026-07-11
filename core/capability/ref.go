package capability

import (
	"fmt"
	"strings"
)

// Ref 是能力的结构化标识,文本形式(4 段):
//
//	cap://<kind>/<domain>/<name>@<version>
//	cap://tool/fs/read_file@1.0
//	cap://skill/research/competitor_report@1
//	cap://store/session/sess
//
// kind 是对象类别;domain 是该 kind 下 name 的归属域(见 domain 不变式:
// callable→源/ns,store/retriever→slot-kind,prompt→prompt 源);version
// 可以是语义版本或通道名(production/staging),空表示任意。
//
// 不变式:kind 段永远精确,通配只出现在 name 段(见 Match)。Key 不含
// version(见 Key)。
type Ref struct {
	Kind    string // tool | skill | component | agent | prompt | store | retriever
	Domain  string // 归属域:源名 / builtin / 存储用途 / prompt 源
	Name    string
	Version string
}

// String 渲染为 URI 形式;version 为空时省略 @ 段。
func (r Ref) String() string {
	s := fmt.Sprintf("cap://%s/%s/%s", r.Kind, r.Domain, r.Name)
	if r.Version != "" {
		s += "@" + r.Version
	}
	return s
}

// Key 返回不含版本的唯一键,用于目录索引与冲突检测。
// 版本共存靠 registry 的 Key→{version→entry},不进 Key。
func (r Ref) Key() string {
	return fmt.Sprintf("%s/%s/%s", r.Kind, r.Domain, r.Name)
}

// ParseRef 解析完整标识或带 * 通配的模式。
// 形式:cap://kind/domain/name[@version];各段可为 * (模式),
// name 段允许前缀通配(如 todo_*)。
func ParseRef(s string) (Ref, error) {
	orig := s
	if !strings.HasPrefix(s, "cap://") {
		return Ref{}, fmt.Errorf("invalid capability ref %q: missing cap:// scheme", orig)
	}
	s = strings.TrimPrefix(s, "cap://")

	var version string
	if i := strings.LastIndex(s, "@"); i >= 0 {
		version = s[i+1:]
		s = s[:i]
	}

	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return Ref{}, fmt.Errorf("invalid capability ref %q: want cap://kind/domain/name", orig)
	}
	r := Ref{Kind: parts[0], Domain: parts[1], Name: parts[2], Version: version}
	for _, seg := range []string{r.Kind, r.Domain, r.Name} {
		if seg == "" {
			return Ref{}, fmt.Errorf("invalid capability ref %q: empty segment", orig)
		}
	}
	// kind 段永远精确(Match 的通配不变式):* 在这里能解析出来却永远
	// 匹配不到任何能力,收下等于承诺做不到的事。
	if strings.Contains(r.Kind, "*") {
		return Ref{}, fmt.Errorf("invalid capability ref %q: the kind segment cannot use wildcards (domain/name can, e.g. cap://tool/*/*)", orig)
	}
	return r, nil
}

// Match 判断 r 是否命中模式 pattern。通配不变式:**kind 段永远精确**;
// domain 与 name 段支持 *(任意)与 name 前缀通配(foo_*);
// pattern.Version 为空表示任意版本。
//
// kind 精确是「kind 优先 dispatch」得以成立的前提——`*`-kind 无法
// 选解析域,故不允许。domain 可通配("某 kind 的全部",如 cap://tool/*/*);
// 「跨 kind 挂载全部」不是通配匹配,是 Catalog.SelectAll 的显式操作。
func (r Ref) Match(pattern Ref) bool {
	return r.Kind == pattern.Kind &&
		matchSeg(r.Domain, pattern.Domain) &&
		matchSeg(r.Name, pattern.Name) &&
		(pattern.Version == "" || pattern.Version == "*" || r.Version == pattern.Version)
}

func matchSeg(val, pat string) bool {
	if pat == "*" || pat == val {
		return true
	}
	if prefix, ok := strings.CutSuffix(pat, "*"); ok {
		return strings.HasPrefix(val, prefix)
	}
	return false
}
