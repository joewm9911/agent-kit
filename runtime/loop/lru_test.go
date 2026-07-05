package loop

import "testing"

func TestLRUEvictsOldest(t *testing.T) {
	c := newLRU[int](2)
	c.put("a", 1)
	c.put("b", 2)
	c.get("a")    // a 变为最近使用
	c.put("c", 3) // 容量 2:淘汰最久未用的 b
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatalf("a lost: %v %v", v, ok)
	}
	if v, ok := c.get("c"); !ok || v != 3 {
		t.Fatalf("c lost: %v %v", v, ok)
	}
}

func TestLRUUpdateInPlace(t *testing.T) {
	c := newLRU[int](2)
	c.put("a", 1)
	c.put("a", 9) // 更新不占新槽
	c.put("b", 2)
	if v, _ := c.get("a"); v != 9 {
		t.Fatalf("update lost: %d", v)
	}
	if c.l.Len() != 2 {
		t.Fatalf("len = %d", c.l.Len())
	}
}

// TestBudgetGateBounded:预算账目有界——超过容量后旧会话被淘汰(重计),
// 活跃会话账目保留,进程不再随会话数无限增长。
func TestBudgetGateBounded(t *testing.T) {
	g := NewBudgetGate(BudgetConfig{})
	g.sessions = newLRU[*spend](8) // 缩小容量便于断言
	for i := 0; i < 32; i++ {
		s, _ := g.check(string(rune('a' + i)))
		s.calls = int64(i)
	}
	if g.sessions.l.Len() != 8 {
		t.Fatalf("sessions should be bounded at 8, got %d", g.sessions.l.Len())
	}
	if calls, _ := g.Spend(string(rune('a'))); calls != 0 {
		t.Fatal("evicted session must restart from zero")
	}
}
