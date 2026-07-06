// Package einoskill 把 agent-kit 的技能物化目录(EnsurePack 产物,
// <work_dir>/agent-kit/.skills/<ns>/<name>@<version>)适配为 eino ADK Skill
// middleware 的 Backend 接口——用 eino ADK 的团队可以直接消费我们分发、
// 锁定(skills.lock)、校验过的技能目录,SKILL.md frontmatter 两边同一份
// (name/description/context/agent/model 全兼容)。
//
//	backend := einoskill.NewBackend(packRoot)
//	handler, _ := adkskill.NewMiddleware(ctx, &adkskill.Config{Backend: backend})
//
// 技能名对外为 "<ns>/<name>"(跨命名空间唯一);执行语义由 eino 侧决定
// (本包只供内容,不参与 agent-kit 的子循环/治理)。
package einoskill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	adkskill "github.com/cloudwego/eino/adk/middlewares/skill"

	"github.com/joewm9911/agent-kit/skill"
)

// NewBackend 以技能物化根目录构造 eino skill.Backend。
func NewBackend(packRoot string) adkskill.Backend {
	return &backend{root: packRoot}
}

type backend struct {
	root string
}

// packDirs 扫描 <root>/<ns>/<name>@<version> 两级布局,返回全部包目录。
func (b *backend) packDirs() ([]skill.PackDir, error) {
	nss, err := os.ReadDir(b.root)
	if err != nil {
		return nil, fmt.Errorf("einoskill: 读取技能根目录 %s: %w", b.root, err)
	}
	var out []skill.PackDir
	for _, ns := range nss {
		if !ns.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(b.root, ns.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(b.root, ns.Name(), e.Name())
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
				continue
			}
			name, version := e.Name(), ""
			if at := strings.LastIndex(name, "@"); at > 0 {
				name, version = name[:at], name[at+1:]
			}
			out = append(out, skill.PackDir{
				Dir: dir, Ref: dir, NS: ns.Name(), Name: name, Version: version,
			})
		}
	}
	return out, nil
}

func toFrontMatter(m *skill.PackManifest) adkskill.FrontMatter {
	return adkskill.FrontMatter{
		Name:        m.NS + "/" + m.Name,
		Description: m.Description,
		Context:     adkskill.ContextMode(m.Context),
		Agent:       m.Agent,
		Model:       m.Model,
	}
}

func (b *backend) List(ctx context.Context) ([]adkskill.FrontMatter, error) {
	pds, err := b.packDirs()
	if err != nil {
		return nil, err
	}
	out := make([]adkskill.FrontMatter, 0, len(pds))
	for _, pd := range pds {
		m, err := skill.LoadManifest(pd)
		if err != nil {
			return nil, err
		}
		out = append(out, toFrontMatter(m))
	}
	return out, nil
}

func (b *backend) Get(ctx context.Context, name string) (adkskill.Skill, error) {
	pds, err := b.packDirs()
	if err != nil {
		return adkskill.Skill{}, err
	}
	for _, pd := range pds {
		if pd.NS+"/"+pd.Name != name {
			continue
		}
		m, err := skill.LoadManifest(pd)
		if err != nil {
			return adkskill.Skill{}, err
		}
		abs, err := filepath.Abs(m.Dir)
		if err != nil {
			abs = m.Dir
		}
		return adkskill.Skill{
			FrontMatter:   toFrontMatter(m),
			Content:       m.Body,
			BaseDirectory: abs,
		}, nil
	}
	return adkskill.Skill{}, fmt.Errorf("einoskill: 技能 %q 不存在(名字形如 <ns>/<name>)", name)
}
