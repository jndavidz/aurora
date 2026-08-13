package yuanbaoweb

import (
	"os"
	"path/filepath"
	"testing"
)

// token 池文件解析:<uskey>\t<cookie> 每行一条,忽略空行/注释/坏行。
func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.txt")
	content := "# comment\n" +
		"uskey1\tcookie header one\n" +
		"\n" +
		"uskey2\tcookie=two; other=3\n" +
		"broken-line-no-tab\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tokens, err := loadTokens(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("tokens = %d, want 2 (comment/blank/broken skipped)", len(tokens))
	}
	if tokens[0].Uskey != "uskey1" || tokens[0].Cookie != "cookie header one" {
		t.Errorf("tokens[0] = %+v", tokens[0])
	}
	if tokens[1].Uskey != "uskey2" || tokens[1].Cookie != "cookie=two; other=3" {
		t.Errorf("tokens[1] = %+v", tokens[1])
	}
}

// NewClient 无 token 文件时构造成功但不注册。
func TestNewClientEmptyPool(t *testing.T) {
	c := NewClient("", filepath.Join(t.TempDir(), "missing.txt"), "")
	if c.HasToken() {
		t.Fatal("HasToken should be false with empty pool")
	}
	if c.agentID != defaultAgentID {
		t.Errorf("agentID = %q, want default %q", c.agentID, defaultAgentID)
	}
	if c.baseURL != defaultBase {
		t.Errorf("baseURL = %q, want default %q", c.baseURL, defaultBase)
	}
}

// cookieValue 从 cookie header 里取指定 cookie。
func TestCookieValue(t *testing.T) {
	h := "hy_user=abc123; _qimei_uuid42=dev42; other=x"
	if got := cookieValue(h, "hy_user"); got != "abc123" {
		t.Errorf("hy_user = %q", got)
	}
	if got := cookieValue(h, "_qimei_uuid42"); got != "dev42" {
		t.Errorf("_qimei_uuid42 = %q", got)
	}
	if got := cookieValue(h, "missing"); got != "" {
		t.Errorf("missing = %q, want empty", got)
	}
}
