package kimiweb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadTokens 验证 token 池文件加载(忽略空行/注释)。
func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "kimi_tokens.txt")
	content := "# comment\n\ntoken_alpha\n\ntoken_beta\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := loadTokens(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0] != "token_alpha" || toks[1] != "token_beta" {
		t.Fatalf("tokens = %v, want [token_alpha token_beta]", toks)
	}
}

// TestNewClientTokenPool 验证 NewClient 从文件加载池并取首个 token。
func TestNewClientTokenPool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(f, []byte("t1\nt2\nt3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient("", f)
	if !c.HasToken() {
		t.Fatal("HasToken = false, want true")
	}
	if c.refreshToken != "t1" {
		t.Fatalf("initial refreshToken = %q, want t1", c.refreshToken)
	}
	if c.PoolSize() != 3 {
		t.Fatalf("PoolSize = %d, want 3", c.PoolSize())
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		seen[c.NextToken()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("NextToken cycle covered %v, want 3 distinct tokens", seen)
	}
}

// TestSetRefreshTokenExtractsIdentity 验证 SetRefreshToken 从 JWT claims 解出账号身份头。
func TestSetRefreshTokenExtractsIdentity(t *testing.T) {
	payload := base64url(`{"device_id":"dev123","ssid":"sess456","sub":"user789"}`)
	tok := "aaa." + payload + ".sig"
	c := NewClient("", "")
	c.SetRefreshToken(tok)
	if c.deviceID != "dev123" || c.sessionID != "sess456" || c.trafficID != "user789" {
		t.Fatalf("identity = %q/%q/%q, want dev123/sess456/user789", c.deviceID, c.sessionID, c.trafficID)
	}
}

// TestSetRefreshTokenBadJWT 验证非 JWT 的 token 不报错、身份头保持空。
func TestSetRefreshTokenBadJWT(t *testing.T) {
	c := NewClient("", "")
	c.SetRefreshToken("not-a-jwt")
	if c.deviceID != "" || c.sessionID != "" || c.trafficID != "" {
		t.Fatalf("identity should stay empty, got %q/%q/%q", c.deviceID, c.sessionID, c.trafficID)
	}
}

// TestCompleteFraming 验证 Complete 请求体是 Connect unary 帧(flags + 4BE 长度 + JSON),
// 且带正确的 Content-Type / Authorization / x-msh-* 头。
func TestCompleteFraming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := buf[:n]
		if len(body) < 5 || body[0] != 0 {
			t.Error("bad framing flags")
			return
		}
		length := int(body[1])<<24 | int(body[2])<<16 | int(body[3])<<8 | int(body[4])
		if length+5 != len(body) {
			t.Errorf("bad framing length: %d != %d", length, len(body)-5)
			return
		}
		if r.Header.Get("Content-Type") != "application/connect+json" {
			t.Error("bad content-type")
		}
		if r.Header.Get("Authorization") != "Bearer at1" {
			t.Error("bad authorization")
		}
		if r.Header.Get("x-msh-device-id") != "dev123" {
			t.Error("bad x-msh-device-id")
		}
		// 收尾帧 flags=2 + len=2 + {}
		w.Header().Set("Content-Type", "application/connect+json")
		w.Write([]byte{0x02, 0x00, 0x00, 0x00, 0x02, '{', '}'})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	c.accessToken = "at1"
	c.deviceID = "dev123"
	resp, err := c.Complete(CompletionRequest{Text: "hi", Scenario: "SCENARIO_K2D5"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// base64url 是测试用的 base64url(无 padding)编码。
func base64url(s string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(s); i += 3 {
		var b [3]byte
		n := copy(b[:], s[i:])
		out = append(out, table[b[0]>>2])
		out = append(out, table[(b[0]&0x3)<<4|b[1]>>4])
		if n > 1 {
			out = append(out, table[(b[1]&0xf)<<2|b[2]>>6])
		}
		if n > 2 {
			out = append(out, table[b[2]&0x3f])
		}
	}
	return string(out)
}
