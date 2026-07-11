package memory

// Search 契约回归(审计批 P1-E):有序、user-first、跨 scope 同键不覆盖。

import (
	"context"
	"testing"
)

func TestSearchUserFirstAndDeterministic(t *testing.T) {
	kv := newTestStore()
	ctx := context.Background()
	// 同键不同值:个人事实必须压过域共识且两条都保留
	if err := kv.Put(ctx, UserScope("u1"), "预算", "上限 100 万"); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, SharedScope, "预算", "默认上限 10 万"); err != nil {
		t.Fatal(err)
	}
	hits, err := kv.Search(ctx, []string{UserScope("u1"), SharedScope}, "预算", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("同键跨 scope 应保留两条,得 %d: %v", len(hits), hits)
	}
	if hits[0].Scope != UserScope("u1") || hits[0].Value != "上限 100 万" {
		t.Fatalf("user 桶必须排前: %v", hits)
	}
	// limit 挤压时留下的是 user 桶
	one, _ := kv.Search(ctx, []string{UserScope("u1"), SharedScope}, "预算", 1)
	if len(one) != 1 || one[0].Scope != UserScope("u1") {
		t.Fatalf("limit=1 应留 user 命中: %v", one)
	}
	// 多关键词分词命中(旧整句子串匹配打不中)
	if err := kv.Put(ctx, SharedScope, "发布流程", "先灰度后全量"); err != nil {
		t.Fatal(err)
	}
	multi, _ := kv.Search(ctx, []string{SharedScope}, "灰度 发布", 5)
	if len(multi) == 0 || multi[0].Key != "发布流程" {
		t.Fatalf("分词检索 miss: %v", multi)
	}
	// 确定序:同一查询重复十次结果一致
	base, _ := kv.Search(ctx, []string{UserScope("u1"), SharedScope}, "预算 流程", 5)
	for i := 0; i < 10; i++ {
		again, _ := kv.Search(ctx, []string{UserScope("u1"), SharedScope}, "预算 流程", 5)
		if len(again) != len(base) {
			t.Fatalf("结果数不稳定")
		}
		for j := range again {
			if again[j] != base[j] {
				t.Fatalf("第 %d 次顺序漂移: %v vs %v", i, again, base)
			}
		}
	}
}
