package loop

// 暂存降级路径(digest.degrade_keep):后端不可用时不再做有损消化,
// 应急保留原文头(缺省 24000),中小结果零损失。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/joewm9911/agent-kit/core/capability"
	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/internal/testmodel"
)

type downKV struct{}

func (downKV) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, fmt.Errorf("backend down")
}
func (downKV) Update(context.Context, string, func([]byte, bool) ([]byte, error), time.Duration) error {
	return fmt.Errorf("backend down")
}
func (downKV) Delete(context.Context, string) error        { return fmt.Errorf("down") }
func (downKV) Scan(context.Context, string) ([]string, error) { return nil, fmt.Errorf("down") }

func TestDigestDegradeKeepOnStoreDown(t *testing.T) {
	ctx := runctx.With(context.Background(), "a", "s")
	ctx = WithResultStore(ctx, NewResultStore(downKV{}, 0))
	m := testmodel.New(schema.AssistantMessage("不该被调用的摘要", nil))

	// 中等结果(6000 rune,> over 4000、< degrade_keep 24000):降级态零损失
	mid := strings.Repeat("数", 6000)
	caps := DigestResults([]capability.Capability{bigTool("mid", mid)}, m, 4000, 0)
	out, err := capability.Invoke(ctx, caps[0], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != mid {
		t.Fatalf("mid-size result must survive intact when store is down, got %d runes with prefix %q", len([]rune(out)), out[:60])
	}
	// 超长结果(30000 rune):保留 degrade_keep 头 + 醒目说明,不做有损消化
	big := strings.Repeat("长", 30000)
	caps = DigestResults([]capability.Capability{bigTool("big", big)}, m, 4000, 0)
	out, err = capability.Invoke(ctx, caps[0], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "暂存不可用") || !strings.Contains(out, "24000") {
		t.Fatalf("oversized degrade must disclose kept/total, got prefix %q", out[:120])
	}
	body := out[strings.Index(out, "]")+1:]
	if n := strings.Count(body, "长"); n != 24000 {
		t.Fatalf("must keep exactly degrade_keep runes, got %d", n)
	}
	// 自定义 degrade_keep 生效
	caps = DigestResults([]capability.Capability{bigTool("big2", big)}, m, 4000, 5000)
	out, _ = capability.Invoke(ctx, caps[0], `{}`)
	body = out[strings.Index(out, "]")+1:]
	if n := strings.Count(body, "长"); n != 5000 {
		t.Fatalf("custom degrade_keep must apply, got %d", n)
	}
}
