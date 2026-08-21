package mimoweb

import "testing"

func TestStripAllCitations(t *testing.T) {
	cases := []struct{ in, want string }{
		{"气温24℃(citation:8)风大", "气温24℃风大"},
		{"(citation:8)", ""},
		{"今天[citation:7][citation:8]多云", "今天多云"},
		{"a[citation:11:https://x.com]b", "ab"},
		{"无标记", "无标记"},
		{"[citation:1", "[citation:1"}, // 未闭合:整体正则不剥(由流式 cleaner 兜底跨帧)
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
		{"气温24℃(citation:8)风大", "气温24℃风大"},
		{"(citation:8)", ""},
		{"a(citation:8)(citation:9)b", "ab"},
		{"无标记文本", "无标记文本"},
	}
	for _, c := range cases {
		cc := newCitationCleaner()
		got := cc.push(c.in)
		if got != c.want {
			t.Errorf("push(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 跨帧
	cc := newCitationCleaner()
	a := cc.push("气温(citation:")
	b := cc.push("8)风大")
	if a != "气温" {
		t.Errorf("跨帧1 = %q, want 气温", a)
	}
	if b != "风大" {
		t.Errorf("跨帧2 = %q, want 风大", b)
	}
}
