package glmweb

import "testing"

func TestTurnSearchStrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"天气很好【turn0search9】。", "天气很好。"},
		{"【turn0search1】【turn0search2】多云", "多云"},
		{"半角[turn0search3]变体", "半角变体"},
		{"无标记文本", "无标记文本"},
	}
	for _, c := range cases {
		if got := turnSearchRe.ReplaceAllString(c.in, ""); got != c.want {
			t.Errorf("strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
