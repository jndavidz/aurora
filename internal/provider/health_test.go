package provider

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// glmJWT 构造仅含 exp 声明的测试 JWT(与 jwtutil 解析前提一致)。
func glmJWT(t *testing.T, expUnix int64) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"exp": expUnix})
	if err != nil {
		t.Fatal(err)
	}
	return "hdr." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// 回归:A3 —— Glm 的凭证健康报告:池 exp 解析、分档、账号数。
func TestGlmCredentialHealth(t *testing.T) {
	dir := t.TempDir()
	far := time.Now().Add(90 * 24 * time.Hour).Unix()
	poolFile := filepath.Join(dir, "glm_tokens.txt")
	content := glmJWT(t, far) + "\ngarbage-line\n"
	if err := os.WriteFile(poolFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := newTestConfig()
	cfg.GlmWebTokens = poolFile
	d := NewGlm(cfg)

	h := d.CredentialHealth()
	if h.Name != "zhipu" {
		t.Fatalf("name = %q", h.Name)
	}
	if h.Accounts != 2 {
		t.Fatalf("accounts = %d, want 2(含垃圾行——池计数反映文件行数)", h.Accounts)
	}
	if h.MinRefreshExpiresAt == nil {
		t.Fatal("应解析出最早过期时间")
	}
	if h.MinRefreshDays == nil || *h.MinRefreshDays < 89 || *h.MinRefreshDays > 91 {
		t.Fatalf("days = %v, want ~90", h.MinRefreshDays)
	}
	if h.Status != "ok" {
		t.Fatalf("90 天应分档 ok, got %q", h.Status)
	}
}

// 回归:A3 —— 空池应报 empty(如 NAS 上 Kimi 池为空的现状)。
func TestGlmCredentialHealthEmptyPool(t *testing.T) {
	dir := t.TempDir()
	poolFile := filepath.Join(dir, "glm_tokens.txt")
	if err := os.WriteFile(poolFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig()
	cfg.GlmWebTokens = poolFile
	d := NewGlm(cfg)

	h := d.CredentialHealth()
	if h.Status != "empty" {
		t.Fatalf("空池应报 empty, got %q", h.Status)
	}
	if !strings.Contains(h.Detail, "重抓") {
		t.Fatalf("detail 应提示重抓, got %q", h.Detail)
	}
}

// 回归:A3 —— 临期凭证应分档 warn(<14 天)。
func TestGlmCredentialHealthWarnTier(t *testing.T) {
	dir := t.TempDir()
	near := time.Now().Add(7 * 24 * time.Hour).Unix()
	poolFile := filepath.Join(dir, "glm_tokens.txt")
	if err := os.WriteFile(poolFile, []byte(glmJWT(t, near)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig()
	cfg.GlmWebTokens = poolFile
	d := NewGlm(cfg)

	h := d.CredentialHealth()
	if h.Status != "warn" {
		t.Fatalf("7 天应分档 warn, got %q", h.Status)
	}
}
