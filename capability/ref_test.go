package capability

import "testing"

func TestParseRef(t *testing.T) {
	r, err := ParseRef("cap://tool.mcp/fs/read_file@1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := Ref{Kind: "tool", Provider: "mcp", Namespace: "fs", Name: "read_file", Version: "1.0"}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	if r.String() != "cap://tool.mcp/fs/read_file@1.0" {
		t.Fatalf("roundtrip failed: %s", r.String())
	}

	if _, err := ParseRef("tool.mcp/fs/read_file"); err == nil {
		t.Fatal("expect error for missing scheme")
	}
	if _, err := ParseRef("cap://toolmcp/fs/read_file"); err == nil {
		t.Fatal("expect error for missing kind.provider dot")
	}
	if _, err := ParseRef("cap://tool.mcp/fs"); err == nil {
		t.Fatal("expect error for missing name segment")
	}
}

func TestRefMatch(t *testing.T) {
	ref := Ref{Kind: "tool", Provider: "mcp", Namespace: "fs", Name: "read_file", Version: "1.0"}
	cases := []struct {
		pattern string
		want    bool
	}{
		{"cap://tool.mcp/fs/read_file@1.0", true},
		{"cap://tool.mcp/fs/read_file", true}, // 无版本 = 任意版本
		{"cap://tool.mcp/fs/*", true},
		{"cap://tool.mcp/fs/read_*", true},
		{"cap://*.*/*/*", true},
		{"cap://tool.mcp/fs/read_file@2.0", false},
		{"cap://tool.mcp/jira/*", false},
		{"cap://skill.mcp/fs/*", false},
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
