package grokweb

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadAccounts 验证账号池文件加载(每行 uid|cookie)。
func TestLoadAccounts(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "grok_cookies.txt")
	content := "# comment\n\nuid-aaa|sso=xxx; sso-rw=yyy\n\nuid-bbb|sso=zzz\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := loadAccounts(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	if accounts[0].UID != "uid-aaa" || !contains(accounts[0].Cookie, "sso-rw=yyy") {
		t.Errorf("account0 = %+v", accounts[0])
	}
	if accounts[1].UID != "uid-bbb" {
		t.Errorf("account1 = %+v", accounts[1])
	}
}

// TestNewClientAccountPool 验证轮询。
func TestNewClientAccountPool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(f, []byte("u1|c1\n\u0020\u0020u2|c2\n\u0020\u0020"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(f)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasAccount() {
		t.Fatal("HasAccount = false, want true")
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		uid, _ := c.NextAccount()
		seen[uid] = true
	}
	if len(seen) != 2 {
		t.Fatalf("NextAccount cycle covered %v, want 2 distinct", seen)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
