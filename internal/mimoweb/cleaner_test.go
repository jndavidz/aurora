package mimoweb

import "testing"

func TestStripAllCitations(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(citation:8)", ""},
		{"[citation:7][citation:8]", ""},
		{"a[citation:11:https://x.com]b", "ab"},
		{"no-marker", "no-marker"},
		{"multi(citation:2)", "multi"},
		{"multi(citation:2", "multi"},
		{"tmpcitation:18wind", "tmpwind"},
		{"tmpcitationwind", "tmpwind"},
		{"abc(citationdef", "abcdef"},
		{"a(citation:7)(。", "a(。"},
	}
	for _, c := range cases {
		if got := stripAllCitations(c.in); got != c.want {
			t.Errorf("stripAllCitations(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCitationCleaner(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"(citation:8)", ""},
		{"a(citation:8)(citation:9)b", "ab"},
		{"no-marker-text", "no-marker-text"},
		// 极细分片残留(2026-08-22 实测):无冒号/无括号
		{"tmpcitation", "tmp"}, // 未闭合,缓冲等待
		{"tmp(citation", "tmp"},
		{"tmpcitation" + string(make([]byte, 120)), "tmpcitation" + string(make([]byte, 120))}, // 超限整体放行
	}
	for _, c := range cases {
		cc := newCitationCleaner()
		got := cc.push(c.in)
		if got != c.want {
			t.Errorf("push(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	cc := newCitationCleaner()
	a := cc.push("temp(citation:")
	b := cc.push("8)wind")
	if a != "temp" {
		t.Errorf("frame1 = %q, want temp", a)
	}
	if b != "wind" {
		t.Errorf("frame2 = %q, want wind", b)
	}
	// 无冒号残片跨帧: "citation" 帧 + 后续
	cc2 := newCitationCleaner()
	a2 := cc2.push("防晒措施citation")
	if a2 != "防晒措施" {
		t.Errorf("bare-cit frame = %q, want 防晒措施", a2)
	}
	b2 := cc2.push(":18)风大")
	if b2 != "风大" {
		t.Errorf("bare-cit frame2 = %q, want 风大", b2)
	}
}
