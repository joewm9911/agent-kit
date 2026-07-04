package capability

import "testing"

func TestParseRef(t *testing.T) {
	r, err := ParseRef("cap://tool/fs/read_file@1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := Ref{Kind: "tool", Domain: "fs", Name: "read_file", Version: "1.0"}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	if r.String() != "cap://tool/fs/read_file@1.0" {
		t.Fatalf("roundtrip failed: %s", r.String())
	}
	if r.Key() != "tool/fs/read_file" {
		t.Fatalf("Key should exclude version: %s", r.Key())
	}

	if _, err := ParseRef("tool/fs/read_file"); err == nil {
		t.Fatal("expect error for missing scheme")
	}
	if _, err := ParseRef("cap://tool/fs"); err == nil {
		t.Fatal("expect error for missing name segment")
	}
	if _, err := ParseRef("cap://tool//read_file"); err == nil {
		t.Fatal("expect error for empty domain segment")
	}
}

func TestRefMatch(t *testing.T) {
	ref := Ref{Kind: "tool", Domain: "fs", Name: "read_file", Version: "1.0"}
	cases := []struct {
		pattern string
		want    bool
	}{
		{"cap://tool/fs/read_file@1.0", true},
		{"cap://tool/fs/read_file", true}, // 无版本 = 任意版本
		{"cap://tool/fs/*", true},         // name 段通配
		{"cap://tool/fs/read_*", true},    // name 段前缀通配
		{"cap://tool/fs/read_file@2.0", false},
		{"cap://tool/jira/*", false}, // domain 精确,不匹配
		{"cap://skill/fs/*", false},  // kind 精确,不匹配
	}
	for _, c := range cases {
		p, err := ParseRef(c.pattern)
		if err != nil {
			t.Fatalf("parse %s: %v", c.pattern, err)
		}
		if got := ref.Match(p); got != c.want {
			t.Errorf("Match(%s) = %v, want %v", c.pattern, got, c.want)
		}
	}
}

// TestKindExact 锁死通配不变式:kind 段永远精确,`*`-kind 不匹配。
func TestKindExact(t *testing.T) {
	ref := Ref{Kind: "tool", Domain: "fs", Name: "x"}
	p, _ := ParseRef("cap://*/*/*")
	if ref.Match(p) {
		t.Fatal("kind wildcard must not match (通配不变式:kind 精确)")
	}
}
