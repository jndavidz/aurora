package session

import "testing"

// clientKey 解析是纯函数 seam:优先级 X-Session-Key 头 → user 字段(剥 #new#)
// → 空(不进池)。信令解析(#new# 前缀)同点收敛,一起验证。
func TestResolveClientKeyPriority(t *testing.T) {
	tests := []struct {
		name    string
		header  string // X-Session-Key 头
		user    string // chat completions user 字段
		wantKey string
		wantNew bool // 是否带显式新会话信令
	}{
		{"头优先于user", "sess-1", "u1", "sess-1", false},
		{"仅头", "sess-1", "", "sess-1", false},
		{"仅user", "", "u1", "u1", false},
		{"user带new前缀", "", "#new#u1", "u1", true},
		{"头与user的new信令叠加", "sess-1", "#new#u1", "sess-1", true},
		{"头带new前缀也算信令", "#new#sess-1", "", "sess-1", true},
		{"两者皆空不进池", "", "", "", false},
		{"user仅new前缀视为无key", "", "#new#", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, isNew := ResolveClientKey(tt.header, tt.user)
			if key != tt.wantKey || isNew != tt.wantNew {
				t.Fatalf("ResolveClientKey(%q, %q) = (%q, %v), want (%q, %v)",
					tt.header, tt.user, key, isNew, tt.wantKey, tt.wantNew)
			}
		})
	}
}
