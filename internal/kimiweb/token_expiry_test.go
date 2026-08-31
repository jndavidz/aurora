package kimiweb

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func mkTestJWT(t *testing.T, expUnix int64) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"exp": expUnix})
	if err != nil {
		t.Fatal(err)
	}
	return "hdr." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// 回归:A1 —— Kimi access_token 仅 ~15 分钟,过期/缺失/解析失败都必须触发重换发。
// ClearAccessToken 走 c.mu,与 RefreshAccessToken 互斥。
func TestAccessTokenNearExpiryAndClear(t *testing.T) {
	c := NewClient("", "")
	if c.HasAccessToken() {
		t.Fatal("初始应无 access_token")
	}
	if !c.AccessTokenNearExpiry(3 * time.Minute) {
		t.Fatal("无票应视为临近过期")
	}

	c.accessToken = mkTestJWT(t, time.Now().Add(-time.Minute).Unix()) // 已过期
	if !c.AccessTokenNearExpiry(3 * time.Minute) {
		t.Fatal("过期票应视为临近过期")
	}

	c.accessToken = mkTestJWT(t, time.Now().Add(15*time.Minute).Unix())
	if c.AccessTokenNearExpiry(3 * time.Minute) {
		t.Fatal("距过期 15min > skew 3min,不应触发")
	}
	if !c.AccessTokenNearExpiry(20 * time.Minute) {
		t.Fatal("距过期 15min < skew 20min,应触发")
	}

	c.accessToken = "garbage"
	if !c.AccessTokenNearExpiry(3 * time.Minute) {
		t.Fatal("exp 解析失败应视为临近过期")
	}

	c.ClearAccessToken()
	if c.HasAccessToken() {
		t.Fatal("ClearAccessToken 后应无票")
	}
}
