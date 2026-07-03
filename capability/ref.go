package capability

import (
	"fmt"
	"strings"
)

// Ref 是能力的结构化标识,文本形式:
//
//	cap://<kind>.<provider>/<namespace>/<name>@<version>
//	cap://tool.mcp/fs/read_file@1.0
//	cap://skill.plan-execute/research/competitor_report@1
//
// kind.provider 回答"是什么、从哪来",namespace 隔离供给源,
// version 可以是语义版本或通道名(production/staging),空表示任意。
type Ref struct {
	Kind      string // tool | model | retriever | memory | skill | agent | flow | prompt
	Provider  string // mcp | http | rpc | local | builtin | a2a | prompt 平台名 ...
	Namespace string // 供给源名(source name)
	Name      string
	Version   string
}

// String 渲染为 URI 形式;version 为空时省略 @ 段。
func (r Ref) String() string {
	s := fmt.Sprintf("cap://%s.%s/%s/%s", r.Kind, r.Provider, r.Namespace, r.Name)
	if r.Version != "" {
		s += "@" + r.Version
	}
	return s
}

// Key 返回不含版本的唯一键,用于目录索引与冲突检测。
func (r Ref) Key() string {
	return fmt.Sprintf("%s.%s/%s/%s", r.Kind, r.Provider, r.Namespace, r.Name)
}

// ParseRef 解析完整标识或带 * 通配的模式。
// 允许的形式:cap://kind.provider/ns/name[@version],各段可为 *;
// name 段允许前缀通配,如 todo_*。
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
		return Ref{}, fmt.Errorf("invalid capability ref %q: want cap://kind.provider/namespace/name", orig)
	}

	kp := strings.SplitN(parts[0], ".", 2)
	if len(kp) != 2 {
		return Ref{}, fmt.Errorf("invalid capability ref %q: first segment must be kind.provider", orig)
	}
	r := Ref{Kind: kp[0], Provider: kp[1], Namespace: parts[1], Name: parts[2], Version: version}
	for _, seg := range []string{r.Kind, r.Provider, r.Namespace, r.Name} {
		if seg == "" {
			return Ref{}, fmt.Errorf("invalid capability ref %q: empty segment", orig)
		}
	}
	return r, nil
}

// Match 判断 r 是否命中模式 pattern。模式各段支持 *(任意)与
// name 段的前缀通配(foo_*);pattern.Version 为空表示任意版本。
func (r Ref) Match(pattern Ref) bool {
	return matchSeg(r.Kind, pattern.Kind) &&
		matchSeg(r.Provider, pattern.Provider) &&
		matchSeg(r.Namespace, pattern.Namespace) &&
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
