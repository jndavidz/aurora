package handler

import "testing"

// TestNormalizeCodingModel 验证 -coding 后缀路由:
// 已知 base 的 coding 变体改写为基础模型;未知/无后缀不动。
func TestNormalizeCodingModel(t *testing.T) {
	cases := []struct {
		model      string
		wantBase   string
		wantCoding bool
	}{
		{"gpt-5-6-coding", "gpt-5-6", true},                     // 已知 coding 变体 → 改写 base
		{"gpt-5-6", "gpt-5-6", false},                           // 无后缀不动
		{"gpt-5-5-coding", "gpt-5-5-coding", false},             // 非白名单 base 不改写
		{"auto", "auto", false},                                 // auto 无 coding
		{"", "", false},                                         // 空模型不动
		{"gpt-5-6-coding-extra", "gpt-5-6-coding-extra", false}, // 非精确后缀不动
	}
	for _, c := range cases {
		base, coding := normalizeCodingModel(c.model)
		if base != c.wantBase || coding != c.wantCoding {
			t.Errorf("normalizeCodingModel(%q) = (%q, %v), want (%q, %v)",
				c.model, base, coding, c.wantBase, c.wantCoding)
		}
	}
}
