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
	// 跨帧开括号: "25℃(" 帧尾括号回退 + 下帧 citation → 一起吞
	cc3 := newCitationCleaner()
	a3 := cc3.push("气温25℃(")
	if a3 != "气温25℃" {
		t.Errorf("paren-retract frame = %q, want 气温25℃", a3)
	}
	b3 := cc3.push("citation:1)风大")
	if b3 != "风大" {
		t.Errorf("paren-cit frame2 = %q, want 风大", b3)
	}
	// 帧尾括号 + 非 citation 下帧:括号应输出
	cc4 := newCitationCleaner()
	a4 := cc4.push("气温25℃(")
	b4 := cc4.push("左右")
	if a4+b4 != "气温25℃(左右" {
		t.Errorf("paren-normal = %q+%q, want 气温25℃(左右", a4, b4)
	}
	// flush:未闭合 citation 丢弃,孤立括号输出
	cc5 := newCitationCleaner()
	_ = cc5.push("abc(citation:")
	if got := cc5.flush(); got != "" {
		t.Errorf("flush pending-cit = %q, want empty", got)
	}
	cc6 := newCitationCleaner()
	_ = cc6.push("abc(")
	if got := cc6.flush(); got != "(" {
		t.Errorf("flush lone-paren = %q, want (", got)
	}
}
