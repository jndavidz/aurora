package qianwenweb

import "testing"

func TestParseCookies(t *testing.T) {
	h := "tongyi_sso_ticket=abc123; x5sec=7b227d; x5sectag=550566; sm_ruid=a|||b"
	cks := parseCookies(h)
	if len(cks) != 4 {
		t.Fatalf("cookies = %d, want 4: %+v", len(cks), cks)
	}
	if cks[0].Name != "tongyi_sso_ticket" || cks[0].Value != "abc123" {
		t.Errorf("cookie[0] = %+v", cks[0])
	}
	if cks[2].Name != "x5sectag" || cks[2].Value != "550566" {
		t.Errorf("cookie[2] = %+v", cks[2])
	}
	if cks[3].Name != "sm_ruid" || cks[3].Value != "a|||b" {
		t.Errorf("cookie[3] = %+v (value contains |||)", cks[3])
	}
	// 空与异常输入
	if got := parseCookies(""); len(got) != 0 {
		t.Errorf("empty header should yield 0 cookies, got %d", len(got))
	}
	if got := parseCookies("nohashvalue"); len(got) != 0 {
		t.Errorf("malformed part should be skipped, got %d", len(got))
	}
}
